//  Copyright 2018 Istio Authors
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package mixer

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	"go.uber.org/multierr"
	"google.golang.org/grpc"

	istio_mixer_v1 "istio.io/api/mixer/v1"
	"istio.io/istio/mixer/adapter"
	"istio.io/istio/mixer/pkg/attribute"
	"istio.io/istio/mixer/pkg/server"
	generatedTmplRepo "istio.io/istio/mixer/template"
	"istio.io/istio/pkg/test/framework/dependency"
	"istio.io/istio/pkg/test/framework/environment"
	"istio.io/istio/pkg/test/framework/environments/kubernetes"
	"istio.io/istio/pkg/test/framework/environments/local"
	"istio.io/istio/pkg/test/framework/internal"
	"istio.io/istio/pkg/test/kube"
)

const (
	telemetryClient = "telemetry"
	policyClient    = "policy"
)

var (
	// LocalComponent is a component for the local environment.
	LocalComponent = &localComponent{}

	// KubeComponent is a component for the Kubernetes environment.
	KubeComponent = &kubeComponent{}

	_ environment.DeployedMixer = &deployedMixer{}
	_ internal.Configurable     = &deployedMixer{}
	_ io.Closer                 = &deployedMixer{}
)

type localComponent struct {
}

// ID implements the component.Component interface.
func (c *localComponent) ID() dependency.Instance {
	return dependency.Mixer
}

// Requires implements the component.Component interface.
func (c *localComponent) Requires() []dependency.Instance {
	return make([]dependency.Instance, 0)
}

// Init implements the component.Component interface.
func (c *localComponent) Init(ctx environment.ComponentContext, deps map[dependency.Instance]interface{}) (interface{}, error) {
	e, ok := ctx.Environment().(*local.Implementation)
	if !ok {
		return nil, fmt.Errorf("unsupported environment: %q", ctx.Environment().EnvironmentID())
	}

	dir, err := e.CreateTmpDirectory("mixer")
	if err != nil {
		return nil, err
	}

	args := server.DefaultArgs()
	args.APIPort = 0
	args.MonitoringPort = 0
	args.ConfigStoreURL = fmt.Sprintf("fs://%s", dir)

	args.Templates = generatedTmplRepo.SupportedTmplInfo
	args.Adapters = adapter.Inventory()

	mi, err := server.New(args)
	if err != nil {
		return nil, err
	}

	go mi.Run()

	conn, err := grpc.Dial(mi.Addr().String(), grpc.WithInsecure())
	if err != nil {
		_ = mi.Close()
		return nil, err
	}

	client := istio_mixer_v1.NewMixerClient(conn)

	return &deployedMixer{
		local: true,
		conn:  conn,
		clients: map[string]istio_mixer_v1.MixerClient{
			telemetryClient: client,
			policyClient:    client,
		},
		args:    args,
		server:  mi,
		workdir: dir,
	}, nil
}

type kubeComponent struct {
}

// ID implements the component.Component interface.
func (c *kubeComponent) ID() dependency.Instance {
	return dependency.Mixer
}

// Requires implements the component.Component interface.
func (c *kubeComponent) Requires() []dependency.Instance {
	return make([]dependency.Instance, 0)
}

// Init implements the component.Component interface.
func (c *kubeComponent) Init(ctx environment.ComponentContext, deps map[dependency.Instance]interface{}) (interface{}, error) {
	e, ok := ctx.Environment().(*kubernetes.Implementation)
	if !ok {
		return nil, fmt.Errorf("unsupported environment: %q", ctx.Environment().EnvironmentID())
	}

	res := &deployedMixer{
		local: false,
		// Use the DefaultArgs to get config identity attribute
		args: server.DefaultArgs(),
	}

	for _, clientType := range []string{telemetryClient, policyClient} {
		pod, err := e.Accessor.WaitForPodBySelectors("istio-system", "istio=mixer", "istio-mixer-type="+clientType)
		if err != nil {
			return nil, err
		}

		// TODO: Right now, simply connect to the telemetry backend at port 9092. We can expand this to connect
		// to policy backend and dynamically figure out ports later.
		// See https://github.com/istio/istio/issues/6175
		options := &kube.PodSelectOptions{
			PodNamespace: pod.Namespace,
			PodName:      pod.Name,
		}
		forwarder, err := kube.PortForward(e.KubeSettings().KubeConfig, options, "", strconv.Itoa(9092))
		if err != nil {
			return nil, err
		}

		conn, err := grpc.Dial(forwarder.Address(), grpc.WithInsecure())
		if err != nil {
			return nil, err
		}

		client := istio_mixer_v1.NewMixerClient(conn)
		res.clients[clientType] = client
		res.forwarders = append(res.forwarders, forwarder)
	}

	return res, nil
}

type deployedMixer struct {
	// Indicates that the component is running in local mode.
	local bool

	conn    *grpc.ClientConn
	clients map[string]istio_mixer_v1.MixerClient

	args    *server.Args
	server  *server.Server
	workdir string

	forwarders []kube.PortForwarder
}

// Report implements DeployedMixer.Report.
func (d *deployedMixer) Report(t testing.TB, attributes map[string]interface{}) {
	t.Helper()

	req := istio_mixer_v1.ReportRequest{
		Attributes: []istio_mixer_v1.CompressedAttributes{
			getAttrBag(attributes)},
	}
	_, err := d.clients[telemetryClient].Report(context.Background(), &req)

	if err != nil {
		t.Fatalf("Error sending report: %v", err)
	}
}

// ApplyConfig implements Configurable.ApplyConfig.
func (d *deployedMixer) ApplyConfig(cfg string) error {
	// This only applies when Mixer is running locally.
	if d.local {
		file := path.Join(d.workdir, "config.yaml")
		err := ioutil.WriteFile(file, []byte(cfg), os.ModePerm)

		if err == nil {
			// TODO: Implement a mechanism for reliably waiting for the configuration to disseminate in the system.
			// We can use CtrlZ to expose the config state of Mixer.
			// See https://github.com/istio/istio/issues/6169 and https://github.com/istio/istio/issues/6170.
			time.Sleep(time.Second * 3)
		}

		return err
	}

	// We shouldn't getting an ApplyConfig for the Kubernetes case.
	return fmt.Errorf("unexpected ApplyConfig call to Mixer component for Kubernetes environment: %s", cfg)
}

// Close implements io.Closer.
func (d *deployedMixer) Close() error {
	var err error
	if d.conn != nil {
		err = multierr.Append(err, d.conn.Close())
		d.conn = nil
	}

	if d.server != nil {
		err = multierr.Append(err, d.server.Close())
		d.server = nil
	}

	for _, fw := range d.forwarders {
		fw.Close()
	}

	return err
}

func getAttrBag(attrs map[string]interface{}) istio_mixer_v1.CompressedAttributes {
	requestBag := attribute.GetMutableBag(nil)
	for k, v := range attrs {
		requestBag.Set(k, v)
	}

	var attrProto istio_mixer_v1.CompressedAttributes
	requestBag.ToProto(&attrProto, nil, 0)
	return attrProto
}
