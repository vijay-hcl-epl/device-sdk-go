// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2019-2020 IOTech Ltd
//
// SPDX-License-Identifier: Apache-2.0

package autoevent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/OneOfOne/xxhash"
	"github.com/edgexfoundry/device-sdk-go/internal/common"
	"github.com/edgexfoundry/device-sdk-go/internal/container"
	"github.com/edgexfoundry/device-sdk-go/internal/handler"
	dsModels "github.com/edgexfoundry/device-sdk-go/pkg/models"
	bootstrapContainer "github.com/edgexfoundry/go-mod-bootstrap/bootstrap/container"
	"github.com/edgexfoundry/go-mod-bootstrap/di"
	"github.com/edgexfoundry/go-mod-core-contracts/clients/logger"
	contract "github.com/edgexfoundry/go-mod-core-contracts/models"
)

type Executor interface {
	Run(ctx context.Context, wg *sync.WaitGroup, dic *di.Container)
	Stop()
}

type executor struct {
	deviceName   string
	autoEvent    contract.AutoEvent
	lastReadings map[string]interface{}
	duration     time.Duration
	stop         bool
	rwmutex      sync.RWMutex
}

// Run triggers this Executor executes the handler for the resource periodically
func (e *executor) Run(ctx context.Context, wg *sync.WaitGroup, dic *di.Container) {
	wg.Add(1)
	defer wg.Done()

	lc := bootstrapContainer.LoggingClientFrom(dic.Get)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.duration):
			if e.stop {
				return
			}

			lc.Debug(fmt.Sprintf("AutoEvent - executing %v", e.autoEvent))
			evt, appErr := readResource(e, dic)
			if appErr != nil {
				lc.Error(fmt.Sprintf("AutoEvent - error occurs when reading resource %s",
					e.autoEvent.Resource))
				continue
			}

			if evt != nil {
				if e.autoEvent.OnChange {
					if compareReadings(e, evt.Readings, evt.HasBinaryValue(), lc) {
						lc.Debug(fmt.Sprintf("AutoEvent - readings are the same as previous one %v", e.lastReadings))
						continue
					}
				}
				if evt.HasBinaryValue() {
					lc.Debug("AutoEvent - pushing CBOR event")
				} else {
					lc.Debug(fmt.Sprintf("AutoEvent - pushing event %s", evt.String()))
				}
				event := &dsModels.Event{Event: evt.Event}
				// Attach origin timestamp for events if none yet specified
				if event.Origin == 0 {
					event.Origin = common.GetUniqueOrigin()
				}
				go common.SendEvent(event, lc, container.CoredataEventClientFrom(dic.Get))
			} else {
				lc.Info(fmt.Sprintf("AutoEvent - no event generated when reading resource %s", e.autoEvent.Resource))
			}
		}
	}
}

func readResource(e *executor, dic *di.Container) (*dsModels.Event, common.AppError) {
	vars := make(map[string]string, 2)
	vars[common.NameVar] = e.deviceName
	vars[common.CommandVar] = e.autoEvent.Resource

	evt, appErr := handler.CommandHandler(vars, "", common.GetCmdMethod, "", dic)
	return evt, appErr
}

func compareReadings(e *executor, readings []contract.Reading, hasBinary bool, lc logger.LoggingClient) bool {
	var identical bool = true
	e.rwmutex.RLock()
	defer e.rwmutex.RUnlock()
	for _, r := range readings {
		switch e.lastReadings[r.Name].(type) {
		case uint64:
			checksum := xxhash.Checksum64(r.BinaryValue)
			if e.lastReadings[r.Name] != checksum {
				e.lastReadings[r.Name] = checksum
				identical = false
			}
		case string:
			v, ok := e.lastReadings[r.Name]
			if !ok || v != r.Value {
				e.lastReadings[r.Name] = r.Value
				identical = false
			}
		case nil:
			if hasBinary && len(r.BinaryValue) > 0 {
				e.lastReadings[r.Name] = xxhash.Checksum64(r.BinaryValue)
			} else {
				e.lastReadings[r.Name] = r.Value
			}
			identical = false
		default:
			lc.Error("Error: unsupported reading type (%T) in autoevent - %v\n", e.lastReadings[r.Name], e.autoEvent)
			identical = false
		}
	}
	return identical
}

// Stop marks this Executor stopped
func (e *executor) Stop() {
	e.stop = true
}

// NewExecutor creates an Executor for an AutoEvent
func NewExecutor(deviceName string, ae contract.AutoEvent) (Executor, error) {
	// check Frequency
	duration, err := time.ParseDuration(ae.Frequency)
	if err != nil {
		return nil, err
	}

	return &executor{
		deviceName:   deviceName,
		autoEvent:    ae,
		lastReadings: make(map[string]interface{}),
		duration:     duration,
		stop:         false}, nil
}
