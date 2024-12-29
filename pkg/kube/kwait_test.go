/*
Copyright The Helm Authors.

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

package kube // import "helm.sh/helm/v3/pkg/kube"

import (
	"errors"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/kubectl/pkg/scheme"
	"sigs.k8s.io/cli-utils/pkg/kstatus/watcher"
	"sigs.k8s.io/cli-utils/pkg/testutil"
)

var podCurrent = `
apiVersion: v1
kind: Pod
metadata:
  name: good-pod
  namespace: ns
status:
  conditions:
  - type: Ready
    status: "True"
  phase: Running
`

var podNoStatus = `
apiVersion: v1
kind: Pod
metadata:
  name: in-progress-pod
  namespace: ns
`

var jobNoStatus = `
apiVersion: batch/v1
kind: Job
metadata:
   name: test
   namespace: qual
   generation: 1
`

var jobComplete = `
apiVersion: batch/v1
kind: Job
metadata:
   name: test
   namespace: qual
   generation: 1
status:
   succeeded: 1
   active: 0
   conditions:
    - type: Complete 
      status: "True"
`

var pausedDeploymentYaml = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: ns-1
  generation: 1
spec:
  paused: true
  replicas: 1
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:1.19.6
        ports:
        - containerPort: 80
`

func getGVR(t *testing.T, mapper meta.RESTMapper, obj *unstructured.Unstructured) schema.GroupVersionResource {
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	require.NoError(t, err)
	return mapping.Resource
}

func TestKWaitJob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		objYamls      []string
		expectErrs    []error
		waitForJobs   bool
		pausedAsReady bool
	}{
		{
			name:       "Job is complete",
			objYamls:   []string{jobComplete},
			expectErrs: nil,
		},
		{
			name:        "Job is not complete",
			objYamls:    []string{jobNoStatus},
			expectErrs:  []error{errors.New("test: Job not ready, status: InProgress"), errors.New("context deadline exceeded")},
			waitForJobs: true,
		},
		{
			name:        "Job is not ready, but we pass wait anyway",
			objYamls:    []string{jobNoStatus},
			expectErrs:  nil,
			waitForJobs: false,
		},
		{
			name:       "Pod is ready",
			objYamls:   []string{podCurrent},
			expectErrs: nil,
		},
		{
			name:       "one of the pods never becomes ready",
			objYamls:   []string{podNoStatus, podCurrent},
			expectErrs: []error{errors.New("in-progress-pod: Pod not ready, status: InProgress"), errors.New("context deadline exceeded")},
		},
		{
			name:          "paused deployment passes",
			objYamls:      []string{pausedDeploymentYaml},
			expectErrs:    nil,
			pausedAsReady: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t)
			fakeClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
			fakeMapper := testutil.NewFakeRESTMapper(
				v1.SchemeGroupVersion.WithKind("Pod"),
				appsv1.SchemeGroupVersion.WithKind("Deployment"),
				batchv1.SchemeGroupVersion.WithKind("Job"),
			)
			objs := []runtime.Object{}
			statusWatcher := watcher.NewDefaultStatusWatcher(fakeClient, fakeMapper)
			for _, podYaml := range tt.objYamls {
				m := make(map[string]interface{})
				err := yaml.Unmarshal([]byte(podYaml), &m)
				assert.NoError(t, err)
				resource := &unstructured.Unstructured{Object: m}
				objs = append(objs, resource)
				gvr := getGVR(t, fakeMapper, resource)
				err = fakeClient.Tracker().Create(gvr, resource, resource.GetNamespace())
				assert.NoError(t, err)
			}
			kwaiter := kstatusWaiter{
				sw:            statusWatcher,
				log:           log.Printf,
				pausedAsReady: tt.pausedAsReady,
			}

			resourceList := ResourceList{}
			for _, obj := range objs {
				list, err := c.Build(objBody(obj), false)
				assert.NoError(t, err)
				resourceList = append(resourceList, list...)
			}

			err := kwaiter.wait(resourceList, time.Second*3, tt.waitForJobs)
			if tt.expectErrs != nil {
				assert.EqualError(t, err, errors.Join(tt.expectErrs...).Error())
				return
			}
			assert.NoError(t, err)
		})
	}
}
