/***** BEGIN LICENSE BLOCK *****
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at http://mozilla.org/MPL/2.0/.
#
# The Initial Developer of the Original Code is the Mozilla Foundation.
# Portions created by the Initial Developer are Copyright (C) 2012
# the Initial Developer. All Rights Reserved.
#
# Contributor(s):
#   Rob Miller (rmiller@mozilla.com)
#
# ***** END LICENSE BLOCK *****/
package pipeline

import (
	"github.com/rafrombrc/go-notify"
	. "heka/message"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Control channel event types used by go-notify
const (
	RELOAD = "reload"
	STOP   = "stop"
)

var PoolSize int

type Plugin interface {
	Init(config interface{}) error
}

type PluginGlobal interface {
	// Called when an event occurs, either RELOAD or STOP
	Event(eventType string)
}

type PluginWithGlobal interface {
	Init(global PluginGlobal, config interface{}) error
	InitOnce(config interface{}) (global PluginGlobal, err error)
}

type PipelinePack struct {
	MsgBytes    []byte
	Message     *Message
	Config      *PipelineConfig
	Decoder     string
	Decoders    map[string]Decoder
	Filters     map[string]Filter
	Outputs     map[string]Output
	Decoded     bool
	Blocked     bool
	FilterChain string
	ChainCount  int
	OutputNames map[string]bool
}

func NewPipelinePack(config *PipelineConfig) *PipelinePack {
	msgBytes := make([]byte, 65536)
	message := Message{}
	outputnames := make(map[string]bool)
	filters := make(map[string]Filter)
	decoders := make(map[string]Decoder)
	outputs := make(map[string]Output)

	pack := &PipelinePack{
		MsgBytes:    msgBytes,
		Message:     &message,
		Config:      config,
		Decoder:     config.DefaultDecoder,
		Decoders:    decoders,
		Decoded:     false,
		Blocked:     false,
		Filters:     filters,
		FilterChain: config.DefaultFilterChain,
		Outputs:     outputs,
		OutputNames: outputnames,
	}
	pack.InitDecoders(config)
	pack.InitFilters(config)
	pack.InitOutputs(config)
	return pack
}

func (self *PipelinePack) InitDecoders(config *PipelineConfig) {
	for name, wrapper := range config.Decoders {
		self.Decoders[name] = wrapper.Create().(Decoder)
	}
}

func (self *PipelinePack) InitFilters(config *PipelineConfig) {
	for name, wrapper := range config.Filters {
		self.Filters[name] = wrapper.Create().(Filter)
	}
}

func (self *PipelinePack) InitOutputs(config *PipelineConfig) {
	for name, wrapper := range config.Outputs {
		self.Outputs[name] = wrapper.Create().(Output)
	}
}

func (self *PipelinePack) Zero() {
	self.MsgBytes = self.MsgBytes[:cap(self.MsgBytes)]
	self.Decoder = self.Config.DefaultDecoder
	self.Decoded = false
	self.Blocked = false
	self.FilterChain = self.Config.DefaultFilterChain
	for outputName, _ := range self.OutputNames {
		delete(self.OutputNames, outputName)
	}
}

func filterProcessor(pipelinePack *PipelinePack) {
	pipelinePack.OutputNames = map[string]bool{}
	config := pipelinePack.Config
	filterChainName, ok := config.Lookup.LocateChain(pipelinePack.Message)
	if ok {
		pipelinePack.FilterChain = filterChainName
	} else {
		filterChainName = pipelinePack.FilterChain
	}
	filterChain, ok := config.FilterChains[filterChainName]
	if !ok {
		log.Printf("Filter chain doesn't exist: %s", filterChainName)
		return
	}
	for _, outputName := range filterChain.Outputs {
		pipelinePack.OutputNames[outputName] = true
	}
	for _, filterName := range filterChain.Filters {
		filter := pipelinePack.Filters[filterName]
		filter.FilterMsg(pipelinePack)
		if pipelinePack.Blocked {
			return
		}
	}
}

func BroadcastEvent(config *PipelineConfig, eventType string) {
	err := notify.Post(eventType, nil)
	if err != nil {
		log.Printf("Error sending %s event:", err.Error())
	}

	var wrapper *PluginWrapper
	for _, wrapper = range config.Filters {
		if wrapper.global != nil {
			wrapper.global.Event(eventType)
		}
	}
	for _, wrapper = range config.Outputs {
		if wrapper.global != nil {
			wrapper.global.Event(eventType)
		}
	}
}

func Run(config *PipelineConfig) {
	log.Println("Starting hekad...")

	// Used for recycling PipelinePack objects
	recycleChan := make(chan *PipelinePack, config.PoolSize+1)

	// Main pipeline function, inputs spawn a goroutine of this for every
	// message
	pipeline := func(pack *PipelinePack) {

		// When finished, reset and recycle the allocated PipelinePack
		defer func() {
			pack.Zero()
			recycleChan <- pack
		}()

		// Decode message if necessary
		if !pack.Decoded {
			decoderName := pack.Decoder
			decoder, ok := pack.Decoders[decoderName]
			if !ok {
				log.Printf("Decoder doesn't exist: %s\n", decoderName)
				return
			}
			if err := decoder.Decode(pack); err != nil {
				log.Printf("Error decoding message (%s): %s", decoderName,
					err)
				return
			} else {
				pack.Decoded = true
			}
		}

		// Run message through the appropriate filters
		filterProcessor(pack)
		if pack.Blocked {
			return
		}

		// Deliver message to appropriate outputs
		for outputName, use := range pack.OutputNames {
			if !use {
				continue
			}
			output, ok := pack.Outputs[outputName]
			if !ok {
				log.Printf("Output doesn't exist: %s\n", outputName)
				continue
			}
			output.Deliver(pack)
		}
	}

	// Initialize all of the PipelinePacks that we'll need
	for i := 0; i < config.PoolSize; i++ {
		recycleChan <- NewPipelinePack(config)
	}

	var wg sync.WaitGroup
	var runner *InputRunner
	timeout := time.Duration(time.Second / 2)
	inputRunners := make(map[string]*InputRunner)

	for name, wrapper := range config.Inputs {
		input := wrapper.Create().(Input)
		runner = &InputRunner{name, input, &timeout}
		inputRunners[name] = runner
		runner.Start(pipeline, recycleChan, &wg)
		wg.Add(1)
		log.Printf("Input started: %s\n", name)
	}

	// wait for sigint
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGHUP)
sigListener:
	for {
		sig := <-sigChan
		switch sig {
		case syscall.SIGHUP:
			BroadcastEvent(config, RELOAD)
		case syscall.SIGINT:
			BroadcastEvent(config, STOP)
			break sigListener
		}
	}

	wg.Wait()
	log.Println("Shutdown complete.")
}