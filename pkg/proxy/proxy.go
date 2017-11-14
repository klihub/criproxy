/*
Copyright 2017 Mirantis

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	// "github.com/davecgh/go-spew/spew"
	// "github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// RuntimeProxy is a gRPC implementation of internalapi.RuntimeService.
type RuntimeProxy struct {
	criVersion CRIVersion
	timeout    time.Duration
	server     *grpc.Server
	conn       *grpc.ClientConn
	clients    []*apiClient
}

type methodInterceptor func(r *RuntimeProxy, ctx context.Context, method string, req, resp CRIObject) (interface{}, error)

// NewRuntimeProxy creates a new internalapi.RuntimeService.
func NewRuntimeProxy(criVersion CRIVersion, addrs []string, connectionTimout time.Duration, hook func()) (*RuntimeProxy, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no sockets specified to connect to")
	}

	r := &RuntimeProxy{criVersion: criVersion}
	r.server = grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		if hook != nil {
			hook()
		}
		return r.intercept(ctx, req, info, handler)
	}))
	for _, addr := range addrs {
		r.clients = append(r.clients, newApiClient(criVersion, addr, connectionTimout))
	}
	if !r.clients[0].isPrimary() {
		return nil, errors.New("the first client should be primary (no id)")
	}
	for _, client := range r.clients[1:] {
		if client.isPrimary() {
			return nil, errors.New("only the first client should be primary (no id)")
		}
	}
	criVersion.Register(r.server)

	return r, nil
}

func (r *RuntimeProxy) Serve(addr string, readyCh chan struct{}) error {
	if err := syscall.Unlink(addr); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	if readyCh != nil {
		close(readyCh)
	}
	return r.server.Serve(ln)
}

func (r *RuntimeProxy) Stop() {
	for _, client := range r.clients {
		client.stop()
	}
	// TODO: check if the server is present
	r.server.GracefulStop()
}

func (r *RuntimeProxy) intercept(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	wrappedReq, wrappedResp, err := r.criVersion.WrapObject(req)
	if err != nil {
		return nil, err
	}
	proxyHandler, found := dispatchTable[info.FullMethod]
	if !found {
		return nil, fmt.Errorf("no handler for method %q", info.FullMethod)
	}
	resp, err := proxyHandler(r, ctx, info.FullMethod, wrappedReq, wrappedResp)
	if err != nil {
		return nil, err
	}
	if wrappedResp, ok := resp.(CRIObject); ok {
		return wrappedResp.Unwrap(), nil
	}
	return resp, nil
}

func (r *RuntimeProxy) primaryClient() (*apiClient, error) {
	if err := <-r.clients[0].connect(); err != nil {
		return nil, err
	}
	return r.clients[0], nil
}

func (r *RuntimeProxy) clientForAnnotations(annotations map[string]string) (*apiClient, error) {
	for _, client := range r.clients {
		if client.annotationsMatch(annotations) {
			if err := <-client.connect(); err != nil {
				return nil, err
			}
			return client, nil
		}
	}
	return nil, fmt.Errorf("criproxy: unknown runtime: %q", annotations[targetRuntimeAnnotationKey])
}

func (r *RuntimeProxy) clientForId(id string) (*apiClient, string, error) {
	client := r.clients[0]
	unprefixed := id
	for _, c := range r.clients[1:] {
		if ok, unpref := c.idPrefixMatches(id); ok {
			c.connect()
			if c.currentState() != clientStateConnected {
				return nil, "", fmt.Errorf("CRI proxy: target runtime is not available")
			}
			client = c
			unprefixed = unpref
			break
		}
	}
	if err := <-client.connect(); err != nil {
		return nil, "", err
	}
	return client, unprefixed, nil
}

func (r *RuntimeProxy) clientForImage(image string, noErrorIfNotConnected bool) (*apiClient, string, error) {
	client := r.clients[0]
	unprefixed := image
	for _, c := range r.clients[1:] {
		if ok, unpref := c.imageMatches(image); ok {
			c.connect()
			// don't wait for additional runtimes
			if c.currentState() != clientStateConnected {
				if noErrorIfNotConnected {
					return nil, "", nil
				}
				return nil, "", fmt.Errorf("CRI proxy: target runtime is not available")
			}
			client = c
			unprefixed = unpref
			break
		}
	}
	if err := <-client.connect(); err != nil {
		return nil, "", err
	}
	return client, unprefixed, nil
}

func (r *RuntimeProxy) fixStreamingUrl(url string) string {
	if strings.HasPrefix(url, "/") {
		// XXX: FIXME!!! Don't hardcode!
		// 10.192.0.3 is only for testing w/kubeadm-dind-cluster (kube-node-1)
		return "http://10.192.0.3:11250" + url
	}
	return url
}

func (r *RuntimeProxy) passToPrimary(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	client, err := r.primaryClient()
	if err != nil {
		return nil, err
	}
	return client.invoke(ctx, method, req, resp)
}

func (r *RuntimeProxy) updateRuntimeConfig(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	var errs []string
	for _, client := range r.clients {
		if client.currentState() != clientStateConnected {
			// This does nothing if the state is clientStateConnecting,
			// otherwise it tries to connect asynchronously
			client.connect()
			continue
		}

		_, err := client.invoke(ctx, method, req, resp)
		if err != nil {
			errs = append(errs, client.handleError(err, false).Error())
		}
	}

	if errs != nil {
		return nil, errors.New(strings.Join(errs, "\n"))
	}

	return resp, nil
}

func (r *RuntimeProxy) listObjects(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	out := resp.(ObjectList)
	clients := r.clients
	var singleClient *apiClient
	useSingleClient := false
	if in, ok := req.(IdFilterObject); ok && in.IdFilter() != "" {
		var unprefixed string
		var err error
		singleClient, unprefixed, err = r.clientForId(in.IdFilter())
		if err != nil {
			return nil, err
		}
		in.SetIdFilter(unprefixed)
		useSingleClient = true
	}

	if in, ok := req.(PodSandboxIdFilterObject); ok && in.PodSandboxIdFilter() != "" {
		anotherClient, unprefixed, err := r.clientForId(in.PodSandboxIdFilter())
		if err != nil {
			return nil, err
		}
		if anotherClient != nil {
			in.SetPodSandboxIdFilter(unprefixed)
			if singleClient == nil {
				singleClient = anotherClient
			} else if singleClient != anotherClient {
				// different id prefixes for sandbox & container
				out.SetItems(nil)
				return resp, nil
			}
		}
		useSingleClient = true
	}

	if in, ok := req.(ImageFilterObject); ok && in.ImageFilter() != "" {
		anotherClient, unprefixed, err := r.clientForImage(in.ImageFilter(), true)
		if err != nil {
			return nil, err
		}
		if anotherClient != nil {
			in.SetImageFilter(unprefixed)
			if singleClient == nil {
				singleClient = anotherClient
			} else if singleClient != anotherClient {
				// this should not really happen because list requests presently
				// don't filter by image and pod / container id at the same time,
				// but let's be sage here
				out.SetItems(nil)
				return resp, nil
			}
		}
		useSingleClient = true
	}

	if useSingleClient {
		if singleClient != nil {
			clients = []*apiClient{singleClient}
		} else {
			// The target client is offline
			out.SetItems(nil)
			return resp, nil
		}
	}

	var items []CRIObject
	for _, client := range clients {
		if client.currentState() != clientStateConnected {
			// This does nothing if the state is clientStateConnecting,
			// otherwise it tries to connect asynchronously
			client.connect()
			continue
		}

		out.SetItems(nil)
		_, err := client.invoke(ctx, method, req, resp)
		if err != nil {
			err = client.handleError(err, true)
			// if the runtime server is gone, let's just skip it
			if err != nil {
				return nil, err
			}
		}
		for _, item := range out.Items() {
			items = append(items, client.addPrefix(item))
		}
	}

	out.SetItems(items)
	return resp, nil

}

func (r *RuntimeProxy) invokePodSandboxMethod(ctx context.Context, method string, req, resp CRIObject) (*apiClient, error) {
	in := req.(PodSandboxIdObject)
	client, unprefixed, err := r.clientForId(in.PodSandboxId())
	if err != nil {
		return nil, err
	}
	in.SetPodSandboxId(unprefixed)
	_, err = client.invoke(ctx, method, req, resp)
	return client, err
}

func (r *RuntimeProxy) invokeContainerMethod(ctx context.Context, method string, req, resp CRIObject) (*apiClient, error) {
	in := req.(ContainerIdObject)
	client, unprefixed, err := r.clientForId(in.ContainerId())
	if err != nil {
		return nil, err
	}
	in.SetContainerId(unprefixed)

	_, err = client.invoke(ctx, method, req, resp)
	return client, err

}

func (r *RuntimeProxy) runPodSandbox(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	client, err := r.clientForAnnotations(req.(RunPodSandboxRequest).GetAnnotations())
	if err != nil {
		return nil, err
	}
	if _, err = client.invoke(ctx, method, req, resp); err == nil {
		out := resp.(RunPodSandboxResponse)
		out.SetPodSandboxId(client.augmentId(out.PodSandboxId()))
	}
	return resp, err
}

func (r *RuntimeProxy) handlePodSandbox(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	_, err := r.invokePodSandboxMethod(ctx, method, req, resp)
	if err == nil {
		if out, ok := resp.(UrlObject); ok {
			out.SetUrl(r.fixStreamingUrl(out.Url()))
		}
	}
	return resp, err
}

func (r *RuntimeProxy) podSandboxStatus(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	client, err := r.invokePodSandboxMethod(ctx, method, req, resp)
	if err != nil {
		return nil, err
	}
	if status := resp.(PodSandboxStatusResponse).Status(); status != nil {
		status.SetId(client.augmentId(status.Id()))
	}
	return resp, nil
}

func (r *RuntimeProxy) createContainer(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	in := req.(CreateContainerRequest)
	client, unprefixed, err := r.clientForId(in.PodSandboxId())
	if err != nil {
		return nil, err
	}
	in.SetPodSandboxId(unprefixed)

	if in.Image() == "" {
		return nil, errors.New("criproxy: no image specified")
	}

	imageClient, unprefixedImage, err := r.clientForImage(in.Image(), false)
	if err != nil {
		return nil, err
	}
	if imageClient != client {
		return nil, fmt.Errorf("criproxy: image %q is for a wrong runtime", in.Image())
	}
	in.SetImage(unprefixedImage)

	_, err = client.invoke(ctx, method, req, resp)
	if err != nil {
		return nil, err
	}

	out := resp.(CreateContainerResponse)
	out.SetContainerId(client.augmentId(out.ContainerId()))
	return out, nil
}

func (r *RuntimeProxy) handleContainer(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	_, err := r.invokeContainerMethod(ctx, method, req, resp)
	if err == nil {
		if out, ok := resp.(UrlObject); ok {
			out.SetUrl(r.fixStreamingUrl(out.Url()))
		}
	}
	return resp, err
}

func (r *RuntimeProxy) containerStatus(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	client, err := r.invokeContainerMethod(ctx, method, req, resp)
	if err != nil {
		return nil, err
	}
	if status := resp.(ContainerStatusResponse).Status(); status != nil {
		status.SetId(client.augmentId(status.Id()))
		status.SetImage(client.imageName(status.Image()))
	}
	return resp, nil
}

func (r *RuntimeProxy) handleImage(ctx context.Context, method string, req, resp CRIObject) (interface{}, error) {
	in := req.(ImageObject)
	client, unprefixed, err := r.clientForImage(in.Image(), true)
	if client == nil {
		// the client is offline
		return resp, nil
	}
	in.SetImage(unprefixed)

	_, err = client.invoke(ctx, method, req, resp)
	if err != nil {
		return nil, err
	}

	if out, ok := resp.(ImageStatusResponse); ok {
		out.SetImage(client.prefixImage(out.Image()))
	}

	if out, ok := resp.(ImageObject); ok {
		out.SetImage(client.imageName(out.Image()))
	}

	return resp, err
}

var dispatchTable map[string]methodInterceptor = map[string]methodInterceptor{
	"/runtime.RuntimeService/Version":             (*RuntimeProxy).passToPrimary,
	"/runtime.RuntimeService/Status":              (*RuntimeProxy).passToPrimary,
	"/runtime.RuntimeService/UpdateRuntimeConfig": (*RuntimeProxy).updateRuntimeConfig,
	"/runtime.RuntimeService/RunPodSandbox":       (*RuntimeProxy).runPodSandbox,
	"/runtime.RuntimeService/ListPodSandbox":      (*RuntimeProxy).listObjects,
	"/runtime.RuntimeService/StopPodSandbox":      (*RuntimeProxy).handlePodSandbox,
	"/runtime.RuntimeService/RemovePodSandbox":    (*RuntimeProxy).handlePodSandbox,
	"/runtime.RuntimeService/PodSandboxStatus":    (*RuntimeProxy).podSandboxStatus,
	"/runtime.RuntimeService/CreateContainer":     (*RuntimeProxy).createContainer,
	"/runtime.RuntimeService/ListContainers":      (*RuntimeProxy).listObjects,
	"/runtime.RuntimeService/StartContainer":      (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/StopContainer":       (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/RemoveContainer":     (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/ContainerStatus":     (*RuntimeProxy).containerStatus,
	"/runtime.RuntimeService/ExecSync":            (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/Exec":                (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/Attach":              (*RuntimeProxy).handleContainer,
	"/runtime.RuntimeService/PortForward":         (*RuntimeProxy).handlePodSandbox,
	"/runtime.ImageService/ListImages":            (*RuntimeProxy).listObjects,
	"/runtime.ImageService/ImageStatus":           (*RuntimeProxy).handleImage,
	"/runtime.ImageService/PullImage":             (*RuntimeProxy).handleImage,
	"/runtime.ImageService/RemoveImage":           (*RuntimeProxy).handleImage,
}

// TODO: tracing requests
// TODO: ContainerStats, ListContainerStats, ImageFsInfo
// TODO: proper streaming url (+ test)
// TODO: try "flaky" test
// TODO: rm commented imports