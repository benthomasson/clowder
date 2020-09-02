/*
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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	cloudredhatcomv1alpha1 "cloud.redhat.com/whippoorwill/v2/apis/cloud.redhat.com/v1alpha1"
	strimzi "cloud.redhat.com/whippoorwill/v2/apis/kafka.strimzi.io/v1beta1"
	"cloud.redhat.com/whippoorwill/v2/controllers/cloud.redhat.com/config"
)

// InsightsAppReconciler reconciles a InsightsApp object
type InsightsAppReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

func (r *InsightsAppReconciler) makeKafka(req *ctrl.Request, iapp *cloudredhatcomv1alpha1.InsightsApp, base *cloudredhatcomv1alpha1.InsightsBase) error {
	ctx := context.Background()

	if len(iapp.Spec.KafkaTopics) > 0 {
		for _, kafkaTopic := range iapp.Spec.KafkaTopics {
			k := strimzi.KafkaTopic{}
			kafkaNamespace := types.NamespacedName{
				Namespace: base.Spec.KafkaNamespace,
				Name:      kafkaTopic.TopicName,
			}

			err := r.Client.Get(ctx, kafkaNamespace, &k)

			update, err := updateOrErr(err)
			if err != nil {
				return err
			}

			k.SetName(kafkaTopic.TopicName)
			k.SetNamespace(base.Spec.KafkaNamespace)
			k.Spec.Replicas = kafkaTopic.Replicas
			k.Spec.Partitions = kafkaTopic.Partitions
			k.Spec.Config = kafkaTopic.Config
			ctrl.Log.Info(fmt.Sprintf("%v", k))
			err = update.Apply(ctx, r.Client, &k)

			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *InsightsAppReconciler) makeService(req *ctrl.Request, iapp *cloudredhatcomv1alpha1.InsightsApp, base *cloudredhatcomv1alpha1.InsightsBase) error {

	ctx := context.Background()
	s := core.Service{}
	err := r.Client.Get(ctx, req.NamespacedName, &s)

	update, err := updateOrErr(err)
	if err != nil {
		return err
	}

	ports := []core.ServicePort{}
	metricsPort := core.ServicePort{Name: "metrics", Port: base.Spec.MetricsPort, Protocol: "TCP"}
	ports = append(ports, metricsPort)

	if iapp.Spec.Web == true {
		webPort := core.ServicePort{Name: "web", Port: base.Spec.WebPort, Protocol: "TCP"}
		ports = append(ports, webPort)
	}

	iapp.SetObjectMeta(&s)
	s.Spec.Selector = iapp.GetLabels()
	s.Spec.Ports = ports

	return update.Apply(ctx, r.Client, &s)
}

func (r *InsightsAppReconciler) makeDeployment(iapp *cloudredhatcomv1alpha1.InsightsApp, base *cloudredhatcomv1alpha1.InsightsBase, d *apps.Deployment) {

	iapp.SetObjectMeta(d)

	d.Spec.Replicas = iapp.Spec.MinReplicas
	d.Spec.Selector = &metav1.LabelSelector{MatchLabels: iapp.GetLabels()}
	d.Spec.Template.Spec.Volumes = iapp.Spec.Volumes
	d.Spec.Template.ObjectMeta.Labels = iapp.GetLabels()

	pullSecretRef := core.LocalObjectReference{Name: "quay-cloudservices-pull"}
	d.Spec.Template.Spec.ImagePullSecrets = []core.LocalObjectReference{pullSecretRef}

	c := core.Container{
		Name:           iapp.ObjectMeta.Name,
		Image:          iapp.Spec.Image,
		Command:        iapp.Spec.Command,
		Args:           iapp.Spec.Args,
		Env:            iapp.Spec.Env,
		Resources:      iapp.Spec.Resources,
		LivenessProbe:  iapp.Spec.LivenessProbe,
		ReadinessProbe: iapp.Spec.ReadinessProbe,
		VolumeMounts:   iapp.Spec.VolumeMounts,
		Ports: []core.ContainerPort{{
			Name:          "metrics",
			ContainerPort: base.Spec.MetricsPort,
		}},
	}

	if iapp.Spec.Web {
		c.Ports = append(c.Ports, core.ContainerPort{
			Name:          "web",
			ContainerPort: base.Spec.WebPort,
		})
	}

	d.Spec.Template.Spec.Containers = []core.Container{c}
}

func (r *InsightsAppReconciler) persistConfig(req ctrl.Request, iapp cloudredhatcomv1alpha1.InsightsApp, c *config.AppConfig) error {

	ctx := context.Background()

	// In any case, we want to overwrite the secret, so this just
	// tests to see if the secret exists
	err := r.Client.Get(ctx, req.NamespacedName, &core.Secret{})

	update, err := updateOrErr(err)
	if err != nil {
		return err
	}

	jsonData, err := json.Marshal(c)
	if err != nil {
		return err
	}

	secret := core.Secret{
		StringData: map[string]string{
			"cdappconfig.json": string(jsonData),
		},
	}

	iapp.SetObjectMeta(&secret)

	return update.Apply(ctx, r.Client, &secret)
}

// +kubebuilder:rbac:groups=cloud.redhat.com,resources=insightsapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.redhat.com,resources=insightsapps/status,verbs=get;update;patch

type updater bool

func (u *updater) Apply(ctx context.Context, cl client.Client, obj runtime.Object) error {
	if *u {
		return cl.Update(ctx, obj)
	}
	return cl.Create(ctx, obj)
}

func updateOrErr(err error) (updater, error) {
	update := updater(err == nil)

	if err != nil && !k8serr.IsNotFound(err) {
		return update, err
	}

	return update, nil
}

// Reconcile fn
func (r *InsightsAppReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	_ = r.Log.WithValues("insightsapp", req.NamespacedName)

	iapp := cloudredhatcomv1alpha1.InsightsApp{}
	err := r.Client.Get(ctx, req.NamespacedName, &iapp)

	if err != nil {
		if k8serr.IsNotFound(err) {
			// TODO: requeue?
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	base := cloudredhatcomv1alpha1.InsightsBase{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: iapp.Namespace,
		Name:      iapp.Spec.Base,
	}, &base)

	if err != nil {
		return ctrl.Result{}, err
	}

	d := apps.Deployment{}
	err = r.Client.Get(ctx, req.NamespacedName, &d)

	update, err := updateOrErr(err)
	if err != nil {
		return ctrl.Result{}, err
	}

	r.makeDeployment(&iapp, &base, &d)

	c := config.New(base.Spec.WebPort, base.Spec.MetricsPort, base.Spec.MetricsPath, config.CloudWatch(
		config.CloudWatchConfig{
			AccessKeyID:     "mah_key",
			SecretAccessKey: "mah_sekret",
			Region:          "us-east-1",
			LogGroup:        iapp.ObjectMeta.Namespace,
		},
	))

	if err = r.persistConfig(req, iapp, c); err != nil {
		return ctrl.Result{}, err
	}

	d.Spec.Template.Spec.Volumes = append(d.Spec.Template.Spec.Volumes, core.Volume{
		Name: "config-secret",
		VolumeSource: core.VolumeSource{
			Secret: &core.SecretVolumeSource{
				SecretName: iapp.ObjectMeta.Name,
			},
		},
	})

	con := &d.Spec.Template.Spec.Containers[0]
	con.VolumeMounts = append(con.VolumeMounts, core.VolumeMount{
		Name:      "config-secret",
		MountPath: "/cdapp/",
	})

	if err = update.Apply(ctx, r.Client, &d); err != nil {
		return ctrl.Result{}, err
	}

	if err = r.makeService(&req, &iapp, &base); err != nil {
		return ctrl.Result{}, err
	}

	if err = r.makeKafka(&req, &iapp, &base); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up wi
func (r *InsightsAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Log.Info("Setting up manager!")
	ctx := context.Background()
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudredhatcomv1alpha1.InsightsApp{}).
		Watches(
			&source.Kind{Type: &cloudredhatcomv1alpha1.InsightsBase{}},
			&handler.EnqueueRequestsFromMapFunc{
				ToRequests: handler.ToRequestsFunc(
					func(a handler.MapObject) []reconcile.Request {
						obj := types.NamespacedName{
							Name:      a.Meta.GetName(),
							Namespace: a.Meta.GetNamespace(),
						}
						// Get the InsightsBase resource

						base := cloudredhatcomv1alpha1.InsightsBase{}
						err := r.Client.Get(ctx, obj, &base)

						if err != nil {
							r.Log.Error(err, "Failed to fetch InsightsBase")
							return nil
						}

						// Get all the InsightsApp resources

						appList := cloudredhatcomv1alpha1.InsightsAppList{}
						r.Client.List(ctx, &appList)

						reqs := []reconcile.Request{}

						// Filter based on base attribute

						for _, app := range appList.Items {
							if app.Spec.Base == base.Name {
								// Add filtered resources to return result
								reqs = append(reqs, reconcile.Request{
									NamespacedName: types.NamespacedName{
										Name:      app.Name,
										Namespace: app.Namespace,
									},
								})
							}
						}

						return reqs
					},
				)},
		).
		Owns(&apps.Deployment{}).
		Owns(&core.Service{}).
		Complete(r)
}