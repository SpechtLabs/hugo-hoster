/*
Copyright 2023.

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
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hugohosterv1alpha1 "github.com/cedi/hugo-hoster/api/v1alpha1"
	pageClient "github.com/cedi/hugo-hoster/pkg/client"
	"github.com/cedi/hugo-hoster/pkg/observability"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
)

// HugoPageReconciler reconciles a HugoPage object
type HugoPageReconciler struct {
	client        client.Client
	pageClient    *pageClient.HugoPageClient
	settingClient *pageClient.SettingsClient
	scheme        *runtime.Scheme
	tracer        trace.Tracer
	settingName   string
}

func NewHugoPageReconciler(client client.Client, pageClient *pageClient.HugoPageClient, settingClient *pageClient.SettingsClient, settingsName string, scheme *runtime.Scheme, tracer trace.Tracer) *HugoPageReconciler {
	return &HugoPageReconciler{
		client:        client,
		pageClient:    pageClient,
		settingClient: settingClient,
		settingName:   settingsName,
		scheme:        scheme,
		tracer:        tracer,
	}
}

// +kubebuilder:rbac:groups="",resources=ConfigMap;Service,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=CronJob;Job,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=Deployment,verbs=get;list;watch;create;update;patch;delete

//+kubebuilder:rbac:groups=hugo-hoster.cedi.dev,resources=Setting,verbs=get;list;watch;create;update;patch;delete

//+kubebuilder:rbac:groups=hugo-hoster.cedi.dev,resources=HugoPages,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=hugo-hoster.cedi.dev,resources=HugoPages/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=hugo-hoster.cedi.dev,resources=HugoPages/finalizers,verbs=update

func (r *HugoPageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
	defer func() {
		reconcilerDuration.WithLabelValues("page_name", req.Name, req.Namespace).Observe(float64(time.Since(startTime).Microseconds()))
	}()

	span := trace.SpanFromContext(ctx)

	// Check if the span was sampled and is recording the data
	if !span.IsRecording() {
		ctx, span = r.tracer.Start(ctx, "HugoPageReconciler.Reconcile")
		defer span.End()
	}

	span.SetAttributes(attribute.String("page_name", req.Name))

	log := observability.NewZapLoggerWithCtxSpanPageName("Reconcile", ctx, span, req.NamespacedName.String())

	// Get Hugo Page from etcd
	page, err := r.pageClient.GetNamespaced(ctx, req.NamespacedName)
	if err != nil || page == nil {
		if k8serrors.IsNotFound(err) {
			observability.RecordInfo(&log, span, "Hugo Page resource not found. Ignoring since object must be deleted")

		} else {
			observability.RecordError(&log, span, err, "Failed to fetch HugoPage resource")
		}
	}

	settings, err := r.settingClient.GetNameNamespace(ctx, r.settingName, req.Namespace)
	if err != nil || settings == nil {
		span.RecordError(err)
		log.Panicw(fmt.Sprintf("Failed to fetch Setting resource. You MUST configure hugo-hoster before deploying a site with the setting object name=%s", r.settingName), zap.Error(err))
		os.Exit(1) // just a failsafe... Panicw should already terminate the program
	}

	_, err = r.upsertConfigMap(ctx, page, settings)
	if err != nil {
		observability.RecordError(&log, span, err, "Failed to upsert page-builder nginx proxy config")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 1 * time.Minute,
		}, err
	}

	_, err = r.upsertPageBuilderCronJob(ctx, page, settings)
	if err != nil {
		observability.RecordError(&log, span, err, "Failed to upsert page-builder CronJob")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 1 * time.Minute,
		}, err
	}

	_, err = r.upsertPageNginxProxy(ctx, page, settings)
	if err != nil {
		observability.RecordError(&log, span, err, "Failed to upsert page-builder nginx proxy deployment")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 1 * time.Minute,
		}, err
	}

	_, err = r.upsertNginxProxyService(ctx, page)
	if err != nil {
		observability.RecordError(&log, span, err, "Failed to upsert nginx-proxy Service")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 1 * time.Minute,
		}, err
	}

	_, err = r.upsertPageIngress(ctx, page, settings)
	if err != nil {
		observability.RecordError(&log, span, err, "Failed to upsert nginx-proxy Ingress")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 1 * time.Minute,
		}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HugoPageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hugohosterv1alpha1.HugoPage{}).
		Owns(&hugohosterv1alpha1.Setting{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.CronJob{}).
		Owns(&apiv1.ConfigMap{}).
		Owns(&apiv1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}

func (r *HugoPageReconciler) upsertPageBuilderCronJob(ctx context.Context, page *hugohosterv1alpha1.HugoPage, settings *hugohosterv1alpha1.Setting) (*batchv1.CronJob, error) {
	startingDeadlineSeconds := int64(100)
	suspend := bool(false)
	successfulJobsHistoryLimit := int32(3)
	failedJobsHistoryLimit := int32(10)

	builderCronJob := &batchv1.CronJob{}
	err := r.client.Get(ctx, types.NamespacedName{Name: page.Name, Namespace: page.Namespace}, builderCronJob)

	builderCronJob.ObjectMeta = metav1.ObjectMeta{
		Name:      page.Name,
		Namespace: page.Namespace,
		Labels:    makeLabels(page, "builder"),
	}

	defaultImage := "ghcr.io/SpechtLabs/page_builder"
	defaultTag := "main"
	imagePullPolicy := apiv1.PullIfNotPresent
	builderContainerImage := fmt.Sprintf("%s:%s", defaultImage, defaultTag)

	if page.Spec.Options != nil && page.Spec.Options.BuildImageOptions != nil {
		if page.Spec.Options.BuildImageOptions.Image != nil {
			builderContainerImage = strings.ReplaceAll(builderContainerImage, defaultImage, *page.Spec.Options.BuildImageOptions.Image)
		}

		if page.Spec.Options.BuildImageOptions.Tag != nil {
			builderContainerImage = strings.ReplaceAll(builderContainerImage, defaultTag, *page.Spec.Options.BuildImageOptions.Tag)
		}

		if page.Spec.Options.BuildImageOptions.ImagePullPolicy != nil {
			imagePullPolicy = *page.Spec.Options.BuildImageOptions.ImagePullPolicy
		}
	}

	builderCronJob.Spec = batchv1.CronJobSpec{
		Schedule:                   page.Spec.CronInterval,
		ConcurrencyPolicy:          "Forbid",
		StartingDeadlineSeconds:    &startingDeadlineSeconds,
		Suspend:                    &suspend,
		SuccessfulJobsHistoryLimit: &successfulJobsHistoryLimit,
		FailedJobsHistoryLimit:     &failedJobsHistoryLimit,
		JobTemplate: batchv1.JobTemplateSpec{
			Spec: batchv1.JobSpec{
				Template: apiv1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: builderCronJob.ObjectMeta.Labels,
					},
					Spec: apiv1.PodSpec{
						RestartPolicy: apiv1.RestartPolicyOnFailure,
						Containers: []apiv1.Container{
							{
								Name:            "page-builder",
								Image:           builderContainerImage,
								ImagePullPolicy: imagePullPolicy,
								Env: []apiv1.EnvVar{
									{
										Name:  "REPO_URL",
										Value: page.Spec.Repository,
									},
									{
										Name:  "PAGE_NAME",
										Value: page.Name,
									},
									{
										Name:  "S3_BUCKET_NAME",
										Value: settings.Spec.S3Config.BucketName,
									},
									{
										Name: "AWS_ACCESS_KEY_ID",
										ValueFrom: &apiv1.EnvVarSource{
											SecretKeyRef: &apiv1.SecretKeySelector{
												LocalObjectReference: apiv1.LocalObjectReference{
													Name: settings.Spec.S3Config.SecretName,
												},
												Key: settings.Spec.S3Config.AccessKeyIDRef,
											},
										},
									},
									{
										Name: "AWS_SECRET_ACCESS_KEY",
										ValueFrom: &apiv1.EnvVarSource{
											SecretKeyRef: &apiv1.SecretKeySelector{
												LocalObjectReference: apiv1.LocalObjectReference{
													Name: settings.Spec.S3Config.SecretName,
												},
												Key: settings.Spec.S3Config.AccessKeyRef,
											},
										},
									},
									{
										Name:  "S3_ENDPOINT",
										Value: settings.Spec.S3Config.Endpoint,
									},
								},
								VolumeMounts: []apiv1.VolumeMount{
									{
										Name:      "hugo-buildcmd",
										MountPath: "/home/builder/build-hugo.sh",
										SubPath:   "build-hugo.sh",
										ReadOnly:  true,
									},
								},
							},
						},
						Volumes: []apiv1.Volume{
							{
								Name: "hugo-buildcmd",
								VolumeSource: apiv1.VolumeSource{
									ConfigMap: &apiv1.ConfigMapVolumeSource{
										LocalObjectReference: apiv1.LocalObjectReference{
											Name: fmt.Sprintf("nginx-proxy-conf-%s", page.Name),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Set Redirect instance as the owner and controller
	ctrl.SetControllerReference(page, builderCronJob, r.scheme)

	if err != nil && k8serrors.IsNotFound(err) {
		if err := r.client.Create(ctx, builderCronJob); err != nil {
			return nil, errors.Wrap(err, "Failed to create new page-builder CronJob")
		}
	} else if err != nil {
		return nil, errors.Wrap(err, "Failed to get page-builder CronJob")
	}

	if err := r.client.Update(ctx, builderCronJob); err != nil {
		return nil, errors.Wrap(err, "Failed to update page-builder CronJob")
	}

	return builderCronJob, nil
}

func (r *HugoPageReconciler) upsertPageIngress(ctx context.Context, page *hugohosterv1alpha1.HugoPage, settings *hugohosterv1alpha1.Setting) (*networkingv1.Ingress, error) {
	ingress := &networkingv1.Ingress{}
	err := r.client.Get(ctx, types.NamespacedName{Name: page.Name, Namespace: page.Namespace}, ingress)

	pathTypePrefix := networkingv1.PathTypePrefix

	ingress.ObjectMeta = metav1.ObjectMeta{
		Name:        page.Name,
		Namespace:   page.Namespace,
		Labels:      makeLabels(page, "nginx-proxy"),
		Annotations: make(map[string]string),
	}

	ingress.Spec = networkingv1.IngressSpec{
		IngressClassName: &settings.Spec.IngressClassName,
		Rules: []networkingv1.IngressRule{
			{
				Host: page.Spec.URL,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathTypePrefix,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: fmt.Sprintf("nginx-proxy-%s-svc", page.Name),
										Port: networkingv1.ServiceBackendPort{
											Number: 80,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if settings.Spec.TLS.Enable {
		// Enable TLS in the ingress Spec
		ingress.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{page.Spec.URL},
				SecretName: fmt.Sprintf("%s-page-secret", strings.ReplaceAll(page.Name, ".", "-")),
			},
		}

		// Add additional annotations based from our TLS spec
		for annotationKey, annotationValue := range settings.Spec.TLS.Annotations {
			ingress.ObjectMeta.Annotations[annotationKey] = annotationValue
		}
	}

	// Set Redirect instance as the owner and controller
	ctrl.SetControllerReference(page, ingress, r.scheme)

	if err != nil && k8serrors.IsNotFound(err) {
		if err := r.client.Create(ctx, ingress); err != nil {
			return nil, errors.Wrap(err, "Failed to create new hugo-page Ingress")
		}
	} else if err != nil {
		return nil, errors.Wrap(err, "Failed to get hugo-page Ingress")
	}

	if err := r.client.Update(ctx, ingress); err != nil {
		return nil, errors.Wrap(err, "Failed to update hugo-page Ingress")
	}

	return ingress, nil
}

func (r *HugoPageReconciler) upsertNginxProxyService(ctx context.Context, page *hugohosterv1alpha1.HugoPage) (*apiv1.Service, error) {
	serviceName := fmt.Sprintf("nginx-proxy-%s-svc", page.Name)

	service := &apiv1.Service{}
	err := r.client.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: page.Namespace}, service)

	service.ObjectMeta = metav1.ObjectMeta{
		Name:      serviceName,
		Namespace: page.Namespace,
		Labels:    makeLabels(page, "nginx-proxy"),
	}

	service.Spec = apiv1.ServiceSpec{
		Selector: makeLabels(page, "nginx-proxy"),
		Ports: []apiv1.ServicePort{
			{
				Name:       "nginx",
				Protocol:   apiv1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(80),
			},
		},
	}

	// Set Redirect instance as the owner and controller
	ctrl.SetControllerReference(page, service, r.scheme)

	if err != nil && k8serrors.IsNotFound(err) {
		if err := r.client.Create(ctx, service); err != nil {
			return nil, errors.Wrap(err, "Failed to create new hugo-page nginx service")
		}
	} else if err != nil {
		return nil, errors.Wrap(err, "Failed to get hugo-page nginx service")
	}

	if err := r.client.Update(ctx, service); err != nil {
		return nil, errors.Wrap(err, "Failed to update hugo-page nginx service")
	}

	return service, nil
}

func (r *HugoPageReconciler) upsertPageNginxProxy(ctx context.Context, page *hugohosterv1alpha1.HugoPage, settings *hugohosterv1alpha1.Setting) (*appsv1.Deployment, error) {

	deploymentName := fmt.Sprintf("nginx-proxy-%s", page.Name)
	oldDeployment := &appsv1.Deployment{}
	newDeployment := &appsv1.Deployment{}

	labels := makeLabels(page, "nginx-proxy")

	err := r.client.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: page.Namespace}, oldDeployment)

	newDeployment.ObjectMeta = metav1.ObjectMeta{
		Name:      deploymentName,
		Namespace: page.Namespace,
		Labels:    labels,
	}

	newDeployment.Spec = appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: labels,
		},
		Replicas: &settings.Spec.NginxProxyReplica,

		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:  "nginx",
						Image: "nginx:alpine",
						Ports: []apiv1.ContainerPort{
							{
								ContainerPort: 80,
							},
						},
						VolumeMounts: []apiv1.VolumeMount{
							{
								Name:      "nginx-config",
								MountPath: "/etc/nginx/nginx.conf",
								SubPath:   "nginx.conf",
								ReadOnly:  true,
							},
						},
					},
				},
				Volumes: []apiv1.Volume{
					{
						Name: "nginx-config",
						VolumeSource: apiv1.VolumeSource{
							ConfigMap: &apiv1.ConfigMapVolumeSource{
								LocalObjectReference: apiv1.LocalObjectReference{
									Name: fmt.Sprintf("nginx-proxy-conf-%s", page.Name),
								},
							},
						},
					},
				},
			},
		},
	}

	// Set Redirect instance as the owner and controller
	ctrl.SetControllerReference(page, newDeployment, r.scheme)

	if err != nil && k8serrors.IsNotFound(err) {
		if err := r.client.Create(ctx, newDeployment); err != nil {
			return nil, errors.Wrap(err, "Failed to create new hugo-page nginx proxy deployment")
		}
	} else if err != nil {
		return nil, errors.Wrap(err, "Failed to get hugo-page nginx proxy deployment")
	}

	// only update the deployment if something has changed compared to the previous version

	if !equalNginxProxyDeployment(*newDeployment, *oldDeployment) {
		if err := r.client.Update(ctx, newDeployment); err != nil {
			return nil, errors.Wrap(err, "Failed to update hugo-page nginx proxy deployment")
		}
	}

	return newDeployment, nil
}

func (r *HugoPageReconciler) upsertConfigMap(ctx context.Context, page *hugohosterv1alpha1.HugoPage, settings *hugohosterv1alpha1.Setting) (*apiv1.ConfigMap, error) {
	configMapName := fmt.Sprintf("nginx-proxy-conf-%s", page.Name)
	configMap := &apiv1.ConfigMap{}

	clientErr := r.client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: page.Namespace}, configMap)

	configMap.ObjectMeta = metav1.ObjectMeta{
		Name:      configMapName,
		Namespace: page.Namespace,
		Labels:    makeLabels(page, "nginx-proxy"),
	}

	// Build the Hugo Build Script
	// git clone --recurse-submodules -j8 --branch "$GIT_BRANCH" "$REPO_URL" "$PAGE_NAME"
	// cd "$PAGE_NAME"
	// hugo
	// aws s3 cp public/ "s3://$S3_BUCKET_NAME/$PAGE_NAME" --recursive --endpoint-url "$S3_ENDPOINT" --cli-connect-timeout 6000

	buildCmd := []string{}
	buildCmd = append(buildCmd, "#!/usr/bin/env bash")
	buildCmd = append(buildCmd, "set -ex")
	buildCmd = append(buildCmd, "git clone --recurse-submodules -j8 --branch \"$GIT_BRANCH\" \"$REPO_URL\" \"$PAGE_NAME\"")
	buildCmd = append(buildCmd, "cd \"$PAGE_NAME\"")
	if page.Spec.Options != nil && len(page.Spec.Options.BuildCommand) > 0 {
		buildCmd = append(buildCmd, page.Spec.Options.BuildCommand)
	} else {
		buildCmd = append(buildCmd, "hugo")
	}
	buildCmd = append(buildCmd, "aws s3 cp public/ \"s3://$S3_BUCKET_NAME/$PAGE_NAME\" --recursive --endpoint-url \"$S3_ENDPOINT\" --cli-connect-timeout 6000")

	// Build the nginx settings
	proxyUrl := settings.Spec.ProxyURL
	if proxyUrl == "" {
		proxyUrl = settings.Spec.S3Config.Endpoint
	}

	template, err := template.New("nginx.conf").Parse(nginxConfTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "Unable to compile nginx.conf template")
	}

	nginxValue := map[string]string{
		"S3_URL":      proxyUrl,
		"BUCKET_NAME": settings.Spec.S3Config.BucketName,
		"PAGE_NAME":   page.Name,
	}

	var nginxConf bytes.Buffer
	template.Execute(&nginxConf, nginxValue)

	configMap.Data = map[string]string{
		"nginx.conf":    nginxConf.String(),
		"build-hugo.sh": strings.Join(buildCmd, "\n"),
	}

	// Set Redirect instance as the owner and controller
	ctrl.SetControllerReference(page, configMap, r.scheme)

	if clientErr != nil && k8serrors.IsNotFound(clientErr) {
		if err := r.client.Create(ctx, configMap); err != nil {
			return nil, errors.Wrap(err, "Failed to create new hugo-page nginx proxy config")
		}
	} else if clientErr != nil {
		return nil, errors.Wrap(err, "Failed to get hugo-page nginx proxy proxy config")
	}

	if err := r.client.Update(ctx, configMap); err != nil {
		return nil, errors.Wrap(err, "Failed to update hugo-page nginx proxy proxy config")
	}

	return configMap, nil
}

func equalNginxProxyDeployment(left, right appsv1.Deployment) bool {
	if !cmp.Equal(left.ObjectMeta.Name, right.ObjectMeta.Name) {
		return false
	}

	if !cmp.Equal(left.ObjectMeta.Namespace, right.ObjectMeta.Namespace) {
		return false
	}

	if !cmp.Equal(left.Spec.Selector.MatchLabels, right.Spec.Selector.MatchLabels) {
		return false
	}

	if !cmp.Equal(left.Spec.Replicas, right.Spec.Replicas) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.ObjectMeta.Labels, right.Spec.Template.ObjectMeta.Labels) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers), len(right.Spec.Template.Spec.Containers)) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers), 1) && !cmp.Equal(len(right.Spec.Template.Spec.Containers), 1) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].Name, right.Spec.Template.Spec.Containers[0].Name) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].Image, right.Spec.Template.Spec.Containers[0].Image) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers[0].Ports), len(right.Spec.Template.Spec.Containers[0].Ports)) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers[0].Ports), 1) && !cmp.Equal(len(right.Spec.Template.Spec.Containers[0].Ports), 1) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort, right.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers[0].VolumeMounts), len(right.Spec.Template.Spec.Containers[0].VolumeMounts)) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Containers[0].VolumeMounts), 1) && !cmp.Equal(len(right.Spec.Template.Spec.Containers[0].VolumeMounts), 1) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name, right.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath, right.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].VolumeMounts[0].SubPath, right.Spec.Template.Spec.Containers[0].VolumeMounts[0].SubPath) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly, right.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Volumes), len(right.Spec.Template.Spec.Volumes)) {
		return false
	}

	if !cmp.Equal(len(left.Spec.Template.Spec.Volumes), 1) && !cmp.Equal(len(right.Spec.Template.Spec.Volumes), 1) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Volumes[0].Name, right.Spec.Template.Spec.Volumes[0].Name) {
		return false
	}

	if !cmp.Equal(left.Spec.Template.Spec.Volumes[0].VolumeSource.ConfigMap.LocalObjectReference.Name, right.Spec.Template.Spec.Volumes[0].VolumeSource.ConfigMap.LocalObjectReference.Name) {
		return false
	}

	return true
}

func makeLabels(page *hugohosterv1alpha1.HugoPage, component string) map[string]string {
	return map[string]string{
		"app":       "hugo-hoster",
		"component": component,
		"page":      page.Name,
	}
}

var nginxConfTemplate = `user  nginx;
worker_processes  1;
error_log  /var/log/nginx/error.log warn;
pid        /var/run/nginx.pid;
events {
	worker_connections  1024;
}
http {
  log_format  main   $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" "$http_x_forwarded_for";
  include            /etc/nginx/mime.types;
  default_type       application/octet-stream;
  access_log         /var/log/nginx/access.log  main;
  sendfile           on;
  keepalive_timeout  65;
  server {
	listen 80;
	listen [::]:80;

	server_name _;
	resolver kube-dns.kube-system.svc.cluster.local valid=5s;

	location /healthz {
	  return 200;
	}

	location / {
	  rewrite ^(.*)\/(?!index\.html)$ $1/index.html last;
	  proxy_pass {{.S3_URL}}/{{.BUCKET_NAME}}/{{.PAGE_NAME}}/;
	  proxy_redirect off;
	  proxy_intercept_errors on;
	  proxy_set_header Host $http_host;
	  proxy_set_header X-Real-IP $remote_addr;
	  proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
	  proxy_set_header X-Forwarded-Proto $scheme;
	}
  }
}
`
