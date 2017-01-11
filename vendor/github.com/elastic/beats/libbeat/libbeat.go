package main

import (
	"os"

	"github.com/ashilokhvostov/beats/libbeat/beat"
	"github.com/ashilokhvostov/beats/libbeat/mock"
)

func main() {
	if err := beat.Run(mock.Name, mock.Version, mock.New()); err != nil {
		os.Exit(1)
	}
}
