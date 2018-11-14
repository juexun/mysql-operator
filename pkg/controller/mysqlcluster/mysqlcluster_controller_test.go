/*
Copyright 2018 Pressinfra SRL
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

// nolint: errcheck
package mysqlcluster

import (
	"fmt"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	gomegatypes "github.com/onsi/gomega/types"

	"golang.org/x/net/context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/presslabs/mysql-operator/pkg/apis/mysql/v1alpha1"
	"github.com/presslabs/mysql-operator/pkg/internal/mysqlcluster"
)

type clusterComponents []runtime.Object

const timeout = time.Second * 2

var _ = Describe("MysqlCluster controller", func() {
	var (
		// channel for incoming reconcile requests
		requests chan reconcile.Request
		// stop channel for controller manager
		stop chan struct{}
		// controller k8s client
		c client.Client
	)

	BeforeEach(func() {
		var recFn reconcile.Reconciler

		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		c = mgr.GetClient()

		recFn, requests = SetupTestReconcile(newReconciler(mgr))
		Expect(add(mgr, recFn)).To(Succeed())

		stop = StartTestManager(mgr)
	})

	AfterEach(func() {
		close(stop)
	})

	Describe("when creating a new mysql cluster", func() {
		var (
			expectedRequest reconcile.Request
			cluster         *mysqlcluster.MysqlCluster
			secret          *corev1.Secret
			components      clusterComponents
		)

		BeforeEach(func() {
			name := fmt.Sprintf("cluster-%d", rand.Int31())
			ns := "default"

			expectedRequest = reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
			}

			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "the-secret", Namespace: ns},
				StringData: map[string]string{
					"ROOT_PASSWORD": "this-is-secret",
				},
			}

			cluster = mysqlcluster.New(&api.MysqlCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: api.MysqlClusterSpec{
					Replicas:   2,
					SecretName: secret.Name,
				},
			})

			components = []runtime.Object{
				&appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql", name),
						Namespace: cluster.Namespace,
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql-nodes", name),
						Namespace: cluster.Namespace,
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql-master", name),
						Namespace: cluster.Namespace,
					},
				},
				&corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql", name),
						Namespace: cluster.Namespace,
					},
				},
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql", name),
						Namespace: cluster.Namespace,
					},
				},
				&policyv1beta1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-mysql", name),
						Namespace: cluster.Namespace,
					},
				},
			}

			Expect(c.Create(context.TODO(), secret)).To(Succeed())
			Expect(c.Create(context.TODO(), cluster.Unwrap())).To(Succeed())

			// Initial reconciliation
			Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
			// Reconcile triggered by components being created and status being
			// updated
			Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))

			// some extra reconcile requests may appear
		drain:
			for {
				select {
				case <-requests:
					continue
				case <-time.After(200 * time.Millisecond):
					break drain
				}
			}

			// We need to make sure that the controller does not create infinite
			// loops
			Consistently(requests).ShouldNot(Receive(Equal(expectedRequest)))
		})

		AfterEach(func() {
			// manually delete all created resources because GC isn't enabled in the test controller plane
			removeAllCreatedResource(c, components)
			c.Delete(context.TODO(), secret)
			c.Delete(context.TODO(), cluster.Unwrap())
		})

		DescribeTable("the reconciler",
			func(nameFmt string, obj runtime.Object) {
				key := types.NamespacedName{
					Name:      fmt.Sprintf(nameFmt, cluster.Name),
					Namespace: cluster.Namespace,
				}

				By("creating the resource when the cluster is created")
				Eventually(func() error { return c.Get(context.TODO(), key, obj) }, timeout).Should(Succeed())

				By("reacreating the resource when it gets deleted")
				// Delete the resource and expect Reconcile to be called
				Expect(c.Delete(context.TODO(), obj)).To(Succeed())
				Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
				Eventually(func() error { return c.Get(context.TODO(), key, obj) }, timeout).Should(Succeed())
			},
			Entry("reconciles the statefulset", "%s-mysql", &appsv1.StatefulSet{}),
			Entry("reconciles the headless service", "%s-mysql-nodes", &corev1.Service{}),
			Entry("reconciles the master service", "%s-mysql-master", &corev1.Service{}),
			Entry("reconciles the config map", "%s-mysql", &corev1.ConfigMap{}),
			Entry("reconciles the pod disruption budget", "%s-mysql", &policyv1beta1.PodDisruptionBudget{}),
		)

		Describe("the reconciler", func() {
			It("should update secret and configmap revision annotations on statefulset", func() {
				sfsKey := types.NamespacedName{
					Name:      cluster.GetNameForResource(mysqlcluster.StatefulSet),
					Namespace: cluster.Namespace,
				}
				statefulSet := &appsv1.StatefulSet{}
				Eventually(func() error {
					return c.Get(context.TODO(), sfsKey, statefulSet)
				}, timeout).Should(Succeed())

				cfgMap := &corev1.ConfigMap{}
				Expect(c.Get(context.TODO(), types.NamespacedName{
					Name:      cluster.GetNameForResource(mysqlcluster.ConfigMap),
					Namespace: cluster.Namespace,
				}, cfgMap)).To(Succeed())
				Expect(c.Get(context.TODO(), types.NamespacedName{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				}, secret)).To(Succeed())

				Expect(statefulSet.Spec.Template.ObjectMeta.Annotations["config_rev"]).To(Equal(cfgMap.ResourceVersion))
				Expect(statefulSet.Spec.Template.ObjectMeta.Annotations["secret_rev"]).To(Equal(secret.ResourceVersion))
			})
			It("should update cluster ready condition", func() {
				// get statefulset
				sfsKey := types.NamespacedName{
					Name:      cluster.GetNameForResource(mysqlcluster.StatefulSet),
					Namespace: cluster.Namespace,
				}
				statefulSet := &appsv1.StatefulSet{}
				Eventually(func() error {
					return c.Get(context.TODO(), sfsKey, statefulSet)
				}, timeout).Should(Succeed())

				// update statefulset condition
				statefulSet.Status.ReadyReplicas = 2
				statefulSet.Status.Replicas = 2
				Expect(c.Status().Update(context.TODO(), statefulSet)).To(Succeed())

				// expect a reconcile event
				Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
				Eventually(requests, timeout).Should(Receive(Equal(expectedRequest)))
				Eventually(getClusterConditions(c, cluster), timeout).Should(haveCondWithStatus(api.ClusterConditionReady, corev1.ConditionTrue))
			})
		})
	})
})

func removeAllCreatedResource(c client.Client, clusterComps []runtime.Object) {
	for _, obj := range clusterComps {
		c.Delete(context.TODO(), obj)
	}
}

// getClusterConditions is a helper func that returns a functions that returns cluster status conditions
func getClusterConditions(c client.Client, cluster *mysqlcluster.MysqlCluster) func() []api.ClusterCondition {
	return func() []api.ClusterCondition {
		cl := &api.MysqlCluster{}
		c.Get(context.TODO(), types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cl)
		return cl.Status.Conditions
	}
}

// haveCondWithStatus is a helper func that returns a matcher to check for an existing condition in a ClusterCondition list.
func haveCondWithStatus(condType api.ClusterConditionType, status corev1.ConditionStatus) gomegatypes.GomegaMatcher {
	return ContainElement(MatchFields(IgnoreExtras, Fields{
		"Type":   Equal(condType),
		"Status": Equal(status),
	}))
}
