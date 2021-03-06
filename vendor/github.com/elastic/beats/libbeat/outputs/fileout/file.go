package fileout

import (
	"encoding/json"

	"github.com/ashilokhvostov/beats/libbeat/common"
	"github.com/ashilokhvostov/beats/libbeat/common/op"
	"github.com/ashilokhvostov/beats/libbeat/logp"
	"github.com/ashilokhvostov/beats/libbeat/outputs"
)

func init() {
	outputs.RegisterOutputPlugin("file", New)
}

type fileOutput struct {
	rotator logp.FileRotator
}

// New instantiates a new file output instance.
func New(cfg *common.Config, _ int) (outputs.Outputer, error) {
	config := defaultConfig
	if err := cfg.Unpack(&config); err != nil {
		return nil, err
	}

	// disable bulk support in publisher pipeline
	cfg.SetInt("flush_interval", -1, -1)
	cfg.SetInt("bulk_max_size", -1, -1)

	output := &fileOutput{}
	if err := output.init(config); err != nil {
		return nil, err
	}
	return output, nil
}

func (out *fileOutput) init(config config) error {
	out.rotator.Path = config.Path
	out.rotator.Name = config.Filename
	if out.rotator.Name == "" {
		out.rotator.Name = config.Index
	}
	logp.Info("File output path set to: %v", out.rotator.Path)
	logp.Info("File output base filename set to: %v", out.rotator.Name)

	rotateeverybytes := uint64(config.RotateEveryKb) * 1024
	logp.Info("Rotate every bytes set to: %v", rotateeverybytes)
	out.rotator.RotateEveryBytes = &rotateeverybytes

	keepfiles := config.NumberOfFiles
	logp.Info("Number of files set to: %v", keepfiles)
	out.rotator.KeepFiles = &keepfiles

	err := out.rotator.CreateDirectory()
	if err != nil {
		return err
	}

	err = out.rotator.CheckIfConfigSane()
	if err != nil {
		return err
	}

	return nil
}

// Implement Outputer
func (out *fileOutput) Close() error {
	return nil
}

func (out *fileOutput) PublishEvent(
	sig op.Signaler,
	opts outputs.Options,
	event common.MapStr,
) error {
	jsonEvent, err := json.Marshal(event)
	if err != nil {
		// mark as success so event is not sent again.
		op.SigCompleted(sig)

		logp.Err("Fail to json encode event(%v): %#v", err, event)
		return err
	}

	err = out.rotator.WriteLine(jsonEvent)
	if err != nil {
		if opts.Guaranteed {
			logp.Critical("Unable to write events to file: %s", err)
		} else {
			logp.Err("Error when writing line to file: %s", err)
		}
	}
	op.Sig(sig, err)
	return err
}
