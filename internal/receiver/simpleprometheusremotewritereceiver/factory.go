// Copyright Splunk, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package simpleprometheusremotewritereceiver

import (
	"context"
	"time"

	"github.com/signalfx/golib/pointer"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		typeString,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, component.StabilityLevelDevelopment),
	)
}

func createMetricsReceiver(
	_ context.Context,
	params receiver.CreateSettings,
	cfg component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	c := cfg.(*Config)
	// promListener, err := prometheus.NewListener()
	return New(params, *c, consumer)
}

func createDefaultConfig() component.Config {
	return &Config{
		ListenAddr: confignet.NetAddr{
			Endpoint:  "127.0.0.1:1234", // TODO hughesjj this.. doesn't seem right to me
			Transport: "tcp",
		},
		ListenPath: "/write",
		Timeout:    pointer.Duration(time.Second * 30),
	}
}