/*
Copyright 2022 The OpenYurt Authors.

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

package podupdater

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	k8sutil "github.com/openyurtio/openyurt/pkg/controller/podupdater/kubernetes"
)

var (
	simpleDaemonSetLabel = map[string]string{"foo": "bar"}
	alwaysReady          = func() bool { return true }
)

// ----------------------------------------------------------------------------------------------------------------
// ----------------------------------------------------new Object--------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------

func newDaemonSet(name string, img string) *appsv1.DaemonSet {
	two := int32(2)
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uuid.NewUUID(),
			Name:      name,
			Namespace: metav1.NamespaceDefault,
		},
		Spec: appsv1.DaemonSetSpec{
			RevisionHistoryLimit: &two,
			// UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
			// 	Type: appsv1.OnDeleteDaemonSetStrategyType,
			// },
			Selector: &metav1.LabelSelector{MatchLabels: simpleDaemonSetLabel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: simpleDaemonSetLabel,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: img}},
				},
			},
		},
	}
}

func newPod(podName string, nodeName string, label map[string]string, ds *appsv1.DaemonSet) *corev1.Pod {
	// Add hash unique label to the pod
	newLabels := label
	var podSpec corev1.PodSpec
	// Copy pod spec from DaemonSet template, or use a default one if DaemonSet is nil
	if ds != nil {
		hash := k8sutil.ComputeHash(&ds.Spec.Template, ds.Status.CollisionCount)
		newLabels = CloneAndAddLabel(label, appsv1.DefaultDaemonSetUniqueLabelKey, hash)
		podSpec = ds.Spec.Template.Spec
	} else {
		podSpec = corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Image:                  "foo/bar",
					TerminationMessagePath: corev1.TerminationMessagePathDefault,
					ImagePullPolicy:        corev1.PullIfNotPresent,
				},
			},
		}
	}

	// Add node name to the pod
	if len(nodeName) > 0 {
		podSpec.NodeName = nodeName
	}

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: podName,
			Labels:       newLabels,
			Namespace:    metav1.NamespaceDefault,
		},
		Spec: podSpec,
	}
	pod.Name = names.SimpleNameGenerator.GenerateName(podName)
	if ds != nil {
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ds, controllerKind)}
	}
	return pod
}

func newNode(name string, ready bool) *corev1.Node {
	cond := corev1.NodeCondition{
		Type:   corev1.NodeReady,
		Status: corev1.ConditionTrue,
	}
	if !ready {
		cond.Status = corev1.ConditionFalse
	}

	return &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceNone,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				cond,
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("100"),
			},
		},
	}
}

// func newNotReadyNode(name string) *corev1.Node {
// 	return &corev1.Node{
// 		TypeMeta: metav1.TypeMeta{APIVersion: "v1"},
// 		ObjectMeta: metav1.ObjectMeta{
// 			Name:      name,
// 			Namespace: metav1.NamespaceNone,
// 		},
// 		Status: corev1.NodeStatus{
// 			Conditions: []corev1.NodeCondition{
// 				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
// 			},
// 			Allocatable: corev1.ResourceList{
// 				corev1.ResourcePods: resource.MustParse("100"),
// 			},
// 		},
// 	}
// }

// ----------------------------------------------------------------------------------------------------------------
// --------------------------------------------------fakeController------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------
type fakeController struct {
	*Controller

	dsStore   cache.Store
	nodeStore cache.Store
	podStore  cache.Store

	fakeRecorder *record.FakeRecorder
}

// ----------------------------------------------------------------------------------------------------------------
// --------------------------------------------------fakePodControl------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------
type fakePodControl struct {
	sync.Mutex
	*k8sutil.FakePodControl
	podStore     cache.Store
	podIDMap     map[string]*v1.Pod
	expectations k8sutil.ControllerExpectationsInterface
}

func newFakePodControl() *fakePodControl {
	podIDMap := make(map[string]*v1.Pod)
	return &fakePodControl{
		FakePodControl: &k8sutil.FakePodControl{},
		podIDMap:       podIDMap,
	}
}

func (f *fakePodControl) DeletePod(ctx context.Context, namespace string, podID string, object runtime.Object) error {
	f.Lock()
	defer f.Unlock()
	if err := f.FakePodControl.DeletePod(ctx, namespace, podID, object); err != nil {
		return fmt.Errorf("failed to delete pod %q", podID)
	}
	pod, ok := f.podIDMap[podID]
	if !ok {
		return fmt.Errorf("pod %q does not exist", podID)
	}
	f.podStore.Delete(pod)
	delete(f.podIDMap, podID)

	ds := object.(*appsv1.DaemonSet)
	dsKey, _ := cache.MetaNamespaceKeyFunc(ds)
	f.expectations.DeletionObserved(dsKey)

	return nil
}

func newTest(initialObjests ...runtime.Object) (*fakeController, *fakePodControl) {
	clientset := fake.NewSimpleClientset(initialObjests...)
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	c := NewController(
		clientset,
		informerFactory.Apps().V1().DaemonSets(),
		informerFactory.Core().V1().Nodes(),
		informerFactory.Core().V1().Pods(),
	)

	fakeRecorder := record.NewFakeRecorder(100)

	c.daemonsetSynced = alwaysReady
	c.nodeSynced = alwaysReady
	c.podSynced = alwaysReady

	podControl := newFakePodControl()
	c.podControl = podControl
	podControl.podStore = informerFactory.Core().V1().Pods().Informer().GetStore()

	fakeCtrl := &fakeController{
		c,
		informerFactory.Apps().V1().DaemonSets().Informer().GetStore(),
		informerFactory.Core().V1().Nodes().Informer().GetStore(),
		informerFactory.Core().V1().Pods().Informer().GetStore(),
		fakeRecorder,
	}

	podControl.expectations = c.expectations
	return fakeCtrl, podControl
}

// ----------------------------------------------------------------------------------------------------------------
// --------------------------------------------------Expectations--------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------

func expectSyncDaemonSets(t *testing.T, fakeCtrl *fakeController, ds *appsv1.DaemonSet, podControl *fakePodControl, expectedDeletes int) error {
	// t.Helper()
	key, err := cache.MetaNamespaceKeyFunc(ds)
	if err != nil {
		return err
	}

	err = fakeCtrl.syncDaemonsetHandler(key)
	if err != nil {
		return err
	}

	err = validateSyncDaemonSets(fakeCtrl, podControl, expectedDeletes)
	if err != nil {
		return err
	}
	return nil
}

// clearExpectations copies the FakePodControl to PodStore and clears the delete expectations.
// func clearExpectations(t *testing.T, fakeCtrl *fakeController, ds *appsv1.DaemonSet, fakePodControl *fakePodControl) {
// 	fakePodControl.Clear()

// 	key, err := cache.MetaNamespaceKeyFunc(ds)
// 	if err != nil {
// 		t.Errorf("Could not get key for daemon.")
// 		return
// 	}
// 	fakeCtrl.expectations.DeleteExpectations(key)
// }

// ----------------------------------------------------------------------------------------------------------------
// -------------------------------------------------------util-----------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------

func setAutoUpgradeAnnotation(ds *appsv1.DaemonSet) {
	metav1.SetMetaDataAnnotation(&ds.ObjectMeta, UpgradeAnnotation, AutoUpgrade)
}

func setOnDelete(ds *appsv1.DaemonSet) {
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
		Type: appsv1.OnDeleteDaemonSetStrategyType,
	}
}

// validateSyncDaemonSets check whether the number of deleted pod and events meet expectations
func validateSyncDaemonSets(fakeCtrl *fakeController, fakePodControl *fakePodControl, expectedDeletes int) error {
	if len(fakePodControl.DeletePodName) != expectedDeletes {
		return fmt.Errorf("Unexpected number of deletes.  Expected %d, got %v\n", expectedDeletes, fakePodControl.DeletePodName)
	}
	return nil
}

func addNodesWithPods(f *fakePodControl, nodeStore cache.Store, podStore cache.Store,
	startIndex, numNodes int, ds *appsv1.DaemonSet, ready bool) ([]*corev1.Node, error) {
	nodes := make([]*corev1.Node, 0)

	for i := startIndex; i < startIndex+numNodes; i++ {
		var nodeName string
		switch ready {
		case true:
			nodeName = fmt.Sprintf("node-ready-%d", i)
		case false:
			nodeName = fmt.Sprintf("node-not-ready-%d", i)

		}

		node := newNode(nodeName, ready)
		err := nodeStore.Add(node)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)

		podName := fmt.Sprintf("pod-%d", i)
		pod := newPod(podName, nodeName, simpleDaemonSetLabel, ds)
		err = podStore.Add(pod)
		if err != nil {
			return nil, err
		}
		f.podIDMap[pod.Name] = pod
	}
	return nodes, nil
}

// ----------------------------------------------------------------------------------------------------------------
// ----------------------------------------------------Test Cases--------------------------------------------------
// ----------------------------------------------------------------------------------------------------------------

type tCase struct {
	name           string
	onDelete       bool
	strategy       string
	nodeNum        int
	readyNodeNum   int
	maxUnavailable int
	turnReady      bool
}

// DaemonSets should place onto NotReady nodes
func TestNotReadyNodeDaemonDoesLaunchPod(t *testing.T) {

	tcases := []tCase{
		{
			name:           "success",
			onDelete:       true,
			strategy:       "auto",
			nodeNum:        3,
			readyNodeNum:   3,
			maxUnavailable: 1,
			turnReady:      false,
		},
		{
			name:           "success with 1 node not-ready",
			onDelete:       true,
			strategy:       "auto",
			nodeNum:        3,
			readyNodeNum:   2,
			maxUnavailable: 1,
			turnReady:      false,
		},
		{
			name:           "success with 2 nodes not-ready",
			onDelete:       true,
			strategy:       "auto",
			nodeNum:        3,
			readyNodeNum:   1,
			maxUnavailable: 1,
			turnReady:      false,
		},
		{
			name:           "success with 2 nodes not-ready, then turn ready",
			onDelete:       true,
			strategy:       "auto",
			nodeNum:        3,
			readyNodeNum:   1,
			maxUnavailable: 1,
			turnReady:      true,
		},
	}

	for _, tcase := range tcases {
		t.Log(tcase.name)
		ds := newDaemonSet("ds", "foo/bar:v1")
		if tcase.onDelete {
			setOnDelete(ds)
		}
		switch tcase.strategy {
		case AutoUpgrade:
			setAutoUpgradeAnnotation(ds)
		}

		fakeCtrl, podControl := newTest(ds)

		// add ready nodes and its pods
		_, err := addNodesWithPods(podControl, fakeCtrl.nodeStore,
			fakeCtrl.podStore, 1, tcase.readyNodeNum, ds, true)
		if err != nil {
			t.Fatal(err)
		}

		// add not-ready nodes and its pods
		notReadyNodes, err := addNodesWithPods(podControl, fakeCtrl.nodeStore,
			fakeCtrl.podStore, 1, tcase.nodeNum-tcase.readyNodeNum, ds, false)
		if err != nil {
			t.Fatal(err)
		}

		ds.Spec.Template.Spec.Containers[0].Image = "foo/bar:v2"

		err = fakeCtrl.dsStore.Add(ds)
		if err != nil {
			t.Fatal(err)
		}

		err = expectSyncDaemonSets(t, fakeCtrl, ds, podControl, tcase.readyNodeNum)
		assert.Equal(t, nil, err)

		if tcase.turnReady {
			fakeCtrl.podControl.(*fakePodControl).Clear()
			for _, node := range notReadyNodes {
				node.Status.Conditions = []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				}
				if err := fakeCtrl.nodeStore.Update(node); err != nil {
					t.Fatal(err)
				}
			}

			err = expectSyncDaemonSets(t, fakeCtrl, ds, podControl, tcase.nodeNum-tcase.readyNodeNum)
			assert.Equal(t, nil, err)
		}
	}
}
