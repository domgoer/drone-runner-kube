// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/drone-runners/drone-runner-kube/nicelog"

	"k8s.io/client-go/util/exec"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	watchtools "k8s.io/client-go/tools/watch"

	"github.com/hashicorp/go-multierror"
)

// Kubernetes implements a Kubernetes pipeline engine.
type Kubernetes struct {
	client *kubernetes.Clientset
	config *rest.Config
}

// NewFromConfig returns a new out-of-cluster engine.
func NewFromConfig(path string) (*Kubernetes, error) {
	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{
		client: clientset,
		config: config,
	}, nil
}

// NewInCluster returns a new in-cluster engine.
func NewInCluster() (*Kubernetes, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{
		client: clientset,
		config: config,
	}, nil
}

// Setup the pipeline environment.
func (k *Kubernetes) Setup(ctx context.Context, spec *Spec) error {
	if spec.PullSecret != nil {
		_, err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Create(toDockerConfigSecret(spec))
		if err != nil {
			return err
		}
	}

	_, err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Create(toSecret(spec))
	if err != nil {
		return err
	}

	_, err = k.client.CoreV1().Pods(spec.PodSpec.Namespace).Create(toPod(spec))
	if err != nil {
		return err
	}

	return nil
}

// Destroy the pipeline environment.
func (k *Kubernetes) Destroy(ctx context.Context, spec *Spec) error {
	var result error

	if spec.PullSecret != nil {
		err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Delete(spec.PullSecret.Name, &metav1.DeleteOptions{})
		if err != nil {
			result = multierror.Append(result, err)
		}
	}

	err := k.client.CoreV1().Secrets(spec.PodSpec.Namespace).Delete(spec.PodSpec.Name, &metav1.DeleteOptions{})
	if err != nil {
		result = multierror.Append(result, err)
	}

	err = k.client.CoreV1().ConfigMaps(spec.PodSpec.Namespace).Delete(spec.PodSpec.Name, &metav1.DeleteOptions{})
	if err != nil {
		result = multierror.Append(result, err)
	}

	err = k.client.CoreV1().Pods(spec.PodSpec.Namespace).Delete(spec.PodSpec.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: int64ptr(0),
	})
	if err != nil {
		result = multierror.Append(result, err)
	}

	return result
}

// Run runs the pipeline step.
func (k *Kubernetes) Run(ctx context.Context, spec *Spec, step *Step, output io.Writer) (*State, error) {
	err := k.waitForReady(ctx, spec, step)
	if err != nil {
		return nil, err
	}

	return k.start(spec, step, output)
}

func (k *Kubernetes) waitFor(ctx context.Context, spec *Spec, conditionFunc func(e watch.Event) (bool, error)) error {
	label := fmt.Sprintf("io.drone.name=%s", spec.PodSpec.Name)
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return k.client.CoreV1().Pods(spec.PodSpec.Namespace).List(metav1.ListOptions{
				LabelSelector: label,
			})
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return k.client.CoreV1().Pods(spec.PodSpec.Namespace).Watch(metav1.ListOptions{
				LabelSelector: label,
			})
		},
	}

	preconditionFunc := func(store cache.Store) (bool, error) {
		_, exists, err := store.Get(&metav1.ObjectMeta{Namespace: spec.PodSpec.Namespace, Name: spec.PodSpec.Name})
		if err != nil {
			return true, err
		}
		if !exists {
			return true, err
		}
		return false, nil
	}

	_, err := watchtools.UntilWithSync(ctx, lw, &v1.Pod{}, preconditionFunc, conditionFunc)
	return err
}

func (k *Kubernetes) waitForReady(ctx context.Context, spec *Spec, step *Step) error {
	return k.waitFor(ctx, spec, func(e watch.Event) (bool, error) {
		switch t := e.Type; t {
		case watch.Added, watch.Modified:
			pod, ok := e.Object.(*v1.Pod)
			if !ok || pod.ObjectMeta.Name != spec.PodSpec.Name {
				return false, nil
			}
			if pod.Status.Phase == v1.PodRunning {
				return true, nil
			}
		}
		return false, nil
	})
}

func (k *Kubernetes) start(spec *Spec, step *Step, output io.Writer) (*State, error) {
	stdoutOutput := nicelog.New(output)
	stderrOutput := nicelog.New(output)

	req := k.client.CoreV1().
		RESTClient().Post().
		Resource("pods").Name(spec.PodSpec.Name).
		Namespace(spec.PodSpec.Namespace).SubResource("exec")
	req.VersionedParams(&v1.PodExecOptions{
		Container: step.ID,
		Command:   []string{"sh", "-c", `echo "$DRONE_SCRIPT" | sh`},
		Stdout:    true,
		Stderr:    true,
	},
		scheme.ParameterCodec,
	)
	executor, err := remotecommand.NewSPDYExecutor(
		k.config, http.MethodPost, req.URL())
	if err != nil {
		return nil, err
	}
	state := &State{
		Exited:    true,
		OOMKilled: false,
	}
	err = executor.Stream(remotecommand.StreamOptions{
		Stdout: stdoutOutput,
		Stderr: stderrOutput,
	})
	stdoutOutput.Flush()
	stderrOutput.Flush()
	if err != nil {
		e, ok := err.(exec.CodeExitError)
		if !ok {
			return nil, err
		}
		state.ExitCode = e.ExitStatus()
	}
	return state, nil
}
