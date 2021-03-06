package redis

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/garyburd/redigo/redis"

	"github.com/ashilokhvostov/beats/libbeat/common"
	"github.com/ashilokhvostov/beats/libbeat/logp"
	"github.com/ashilokhvostov/beats/libbeat/outputs/transport"
)

var (
	versionRegex = regexp.MustCompile(`redis_version:(\d+).(\d+)`)
)

type publishFn func(dest []byte, events []common.MapStr) ([]common.MapStr, error)

type client struct {
	*transport.Client
	dataType redisDataType
	db       int
	list     []byte
	password string
	publish  publishFn
}

type redisDataType uint16

const (
	redisListType redisDataType = iota
	redisChannelType
)

func newClient(tc *transport.Client, pass string, db int, dest []byte, dt redisDataType) *client {
	return &client{
		Client:   tc,
		password: pass,
		db:       db,
		dataType: dt,
		list:     dest,
	}
}

func (c *client) Connect(to time.Duration) error {
	debugf("connect")
	err := c.Client.Connect()
	if err != nil {
		return err
	}

	conn := redis.NewConn(c.Client, to, to)
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	if err = initRedisConn(conn, c.password, c.db); err == nil {
		c.publish, err = makePublish(conn, c.dataType)
	}
	return err
}

func initRedisConn(c redis.Conn, pwd string, db int) error {
	if pwd != "" {
		if _, err := c.Do("AUTH", pwd); err != nil {
			return err
		}
	}

	if _, err := c.Do("PING"); err != nil {
		return err
	}

	if db != 0 {
		if _, err := c.Do("SELECT", db); err != nil {
			return err
		}
	}

	return nil
}

func (c *client) Close() error {
	debugf("close connection")
	return c.Client.Close()
}

func (c *client) PublishEvent(event common.MapStr) error {
	_, err := c.PublishEvents([]common.MapStr{event})
	return err
}

func (c *client) PublishEvents(events []common.MapStr) ([]common.MapStr, error) {
	return c.publish(c.list, events)
}

func makePublish(conn redis.Conn, dt redisDataType) (publishFn, error) {
	if dt == redisChannelType {
		return makePublishPUBLISH(conn)
	}
	return makePublishRPUSH(conn)
}

func makePublishRPUSH(conn redis.Conn) (publishFn, error) {
	var major, minor int
	var versionRaw [][]byte

	respRaw, err := conn.Do("INFO")
	resp, err := redis.Bytes(respRaw, err)
	if err != nil {
		return nil, err
	}

	versionRaw = versionRegex.FindSubmatch(resp)
	if versionRaw == nil {
		err = errors.New("unable to read redis_version")
		return nil, err
	}

	major, err = strconv.Atoi(string(versionRaw[1]))
	if err != nil {
		return nil, err
	}

	minor, err = strconv.Atoi(string(versionRaw[2]))
	if err != nil {
		return nil, err
	}

	// Check Redis version number choosing the method
	// how RPUSH shall be used. With version 2.4 RPUSH
	// can accept multiple values at once turning RPUSH
	// into batch like call instead of relying on pipelining.
	//
	// Versions 1.0 to 2.3 only accept one value being send with
	// RPUSH requiring pipelining.
	//
	// See: http://redis.io/commands/rpush
	multiValue := major > 2 || (major == 2 && minor >= 4)
	if multiValue {
		return publishEventsBulk(conn, "RPUSH"), nil
	}
	return publishEventsPipeline(conn, "RPUSH"), nil
}

func makePublishPUBLISH(conn redis.Conn) (publishFn, error) {
	return publishEventsPipeline(conn, "PUBLISH"), nil
}

func publishEventsBulk(conn redis.Conn, command string) publishFn {
	return func(dest []byte, events []common.MapStr) ([]common.MapStr, error) {
		args := make([]interface{}, 1, len(events)+1)
		args[0] = dest

		events, args = serializeEvents(args, 1, events)
		if (len(args) - 1) == 0 {
			return nil, nil
		}

		// RPUSH returns total length of list -> fail and retry all on error
		_, err := conn.Do(command, args...)
		if err != nil {
			logp.Err("Failed to %v to redis list (%v) with %v", command, err)
			return events, err
		}

		return nil, nil
	}
}

func publishEventsPipeline(conn redis.Conn, command string) publishFn {
	return func(dest []byte, events []common.MapStr) ([]common.MapStr, error) {
		var args [2]interface{}
		args[0] = dest

		serialized := make([]interface{}, 0, len(events))
		events, serialized = serializeEvents(serialized, 0, events)
		if len(serialized) == 0 {
			return nil, nil
		}

		for _, event := range serialized {
			args[1] = event
			if err := conn.Send(command, args[:]...); err != nil {
				logp.Err("Failed to execute %v: %v", command, err)
				return events, err
			}
		}

		if err := conn.Flush(); err != nil {
			return events, err
		}

		failed := events[:0]
		var lastErr error
		for i := range serialized {
			_, err := conn.Receive()
			if err != nil {
				if _, ok := err.(redis.Error); ok {
					logp.Err("Failed to %v event to list (%v) with %v",
						command, dest, err)
					failed = append(failed, events[i])
					lastErr = err
				} else {
					logp.Err("Failed to %v multiple events to list (%v) with %v",
						command, dest, err)
					failed = append(failed, events[i:]...)
					lastErr = err
					break
				}
			}
		}
		return failed, lastErr
	}
}

func serializeEvents(
	to []interface{},
	i int,
	events []common.MapStr,
) ([]common.MapStr, []interface{}) {
	okEvents := events
	for _, event := range events {
		jsonEvent, err := json.Marshal(event)
		if err != nil {
			logp.Err("Failed to convert the event to JSON (%v): %#v", err, event)
			goto failLoop
		}
		to = append(to, jsonEvent)
		i++
	}
	return okEvents, to

failLoop:
	okEvents = events[:i]
	restEvents := events[i+1:]
	for _, event := range restEvents {
		jsonEvent, err := json.Marshal(event)
		if err != nil {
			logp.Err("Failed to convert the event to JSON (%v): %#v", err, event)
			i++
			continue
		}
		to = append(to, jsonEvent)
		i++
	}

	return okEvents, to
}
