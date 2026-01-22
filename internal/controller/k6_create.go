package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/grafana/k6-operator/api/v1alpha1"
	"github.com/grafana/k6-operator/pkg/cloud"
	"github.com/grafana/k6-operator/pkg/resources/jobs"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func newLabels(name string) map[string]string {
	return map[string]string{
		"app":   "k6",
		"k6_cr": name,
	}
}

// CreateJobs creates jobs that will spawn k6 pods for distributed test
func CreateJobs(ctx context.Context, log logr.Logger, k6 *v1alpha1.TestRun, r *TestRunReconciler) (ctrl.Result, error) {
	// needed for cloud tests
	tokenInfo := cloud.NewTokenInfo(k6.GetSpec().Token, k6.NamespacedName().Namespace)

	if v1alpha1.IsTrue(k6, v1alpha1.CloudTestRun) && v1alpha1.IsTrue(k6, v1alpha1.CloudTestRunCreated) {
		log = log.WithValues("testRunId", k6.GetStatus().TestRunID)

		err := tokenInfo.Load(ctx, log, r.Client)
		if err != nil {
			// An error here means a very likely mis-configuration of the token.
			// TODO: update status to error to let a user know quicker
			log.Error(err, "A problem while getting token.")
			return ctrl.Result{}, nil
		}
		if !tokenInfo.Ready {
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil
		}
	}

	log.Info("Creating test jobs")

	if res, recheck, err := createJobSpecs(ctx, log, k6, r, tokenInfo); err != nil {
		if v1alpha1.IsTrue(k6, v1alpha1.CloudTestRun) {
			events := cloud.ErrorEvent(cloud.K6OperatorStartError).
				WithDetail(fmt.Sprintf("Failed to create runner jobs: %v", err)).
				WithAbort()
			cloud.SendTestRunEvents(r.k6CloudClient, k6.TestRunID(), log, events)
		}

		return res, err
	} else if recheck {
		return res, nil
	}

	log.Info("Changing stage of TestRun status to created")
	k6.GetStatus().Stage = "created"

	if updateHappened, err := r.UpdateStatus(ctx, k6, log); err != nil {
		return ctrl.Result{}, err
	} else if updateHappened {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

func createJobSpecs(ctx context.Context, log logr.Logger, k6 *v1alpha1.TestRun, r *TestRunReconciler, tokenInfo *cloud.TokenInfo) (ctrl.Result, bool, error) {
	found := &batchv1.Job{}
	namespacedName := types.NamespacedName{
		Name:      fmt.Sprintf("%s-1", k6.NamespacedName().Name),
		Namespace: k6.NamespacedName().Namespace,
	}

	if err := r.Get(ctx, namespacedName, found); err == nil || !errors.IsNotFound(err) {
		if err == nil {
			err = fmt.Errorf("job with the name %s exists; make sure you've deleted your previous run", namespacedName.Name)
		}
		log.Info(err.Error())

		// is it possible to implement this delay with resourceVersion of the job?

		t, condUpdated := v1alpha1.LastUpdate(k6, v1alpha1.CloudTestRun)
		// If condition is unknown then resource hasn't been updated with `k6 inspect` results.
		// If it has been updated but very recently, wait a bit before throwing an error.
		if v1alpha1.IsUnknown(k6, v1alpha1.CloudTestRun) || !condUpdated || time.Since(t).Seconds() <= 30 {
			// try again before returning an error
			return ctrl.Result{RequeueAfter: time.Second * 10}, true, nil
		}

		return ctrl.Result{}, false, err
	}

	var segmentConfigmap *corev1.ConfigMap
	var err error

	if segmentConfigmap = newSharedRunnedConfigmap(k6); segmentConfigmap != nil {
		if err = ctrl.SetControllerReference(k6, segmentConfigmap, r.Scheme); err != nil {
			return ctrl.Result{}, false, err
		}
		if err = r.Create(ctx, segmentConfigmap); err != nil {
			return ctrl.Result{}, false, err
		}
	}

	for i := 1; i <= int(k6.GetSpec().Parallelism); i++ {
		if err := launchTest(ctx, k6, i, log, r, tokenInfo, segmentConfigmap); err != nil {
			return ctrl.Result{}, false, err
		}
	}
	return ctrl.Result{}, false, nil
}

func newSharedRunnedConfigmap(k6 *v1alpha1.TestRun) *corev1.ConfigMap {
	total := int(k6.GetSpec().Parallelism)

	if total <= 1 {
		return nil
	}

	executionSegmentSequence := []string{}

	for i := 1; i < total; i++ {
		executionSegmentSequence = append(executionSegmentSequence, fmt.Sprintf("%d/%d", i, total))
	}

	jsonData, _ := json.Marshal(map[string]interface{}{
		"executionSegmentSequence": fmt.Sprintf("0,%s,1", strings.Join(executionSegmentSequence, ",")),
	})

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s", k6.NamespacedName().Name),
			Namespace: k6.NamespacedName().Namespace,
			Labels:    newLabels(k6.NamespacedName().Name),
		},
		Data: map[string]string{
			"config.json": string(jsonData),
		},
	}
}

func launchTest(ctx context.Context, k6 *v1alpha1.TestRun, index int, log logr.Logger, r *TestRunReconciler, tokenInfo *cloud.TokenInfo, segmentConfigMap *corev1.ConfigMap) error {
	var job *batchv1.Job
	var service *corev1.Service
	var err error

	msg := fmt.Sprintf("Launching k6 test #%d", index)
	log.Info(msg)

	if job, err = jobs.NewRunnerJob(k6, index, tokenInfo, segmentConfigMap); err != nil {
		log.Error(err, "Failed to generate k6 test job")
		return err
	}

	log.Info(fmt.Sprintf("Runner job is ready to start with image `%s` and command `%s`",
		job.Spec.Template.Spec.Containers[0].Image, job.Spec.Template.Spec.Containers[0].Command))

	if err = ctrl.SetControllerReference(k6, job, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for job")
		return err
	}

	if err = r.Create(ctx, job); err != nil {
		log.Error(err, "Failed to launch k6 test")
		return err
	}

	if !k6.GetSpec().UseDirectPodIPs {
		if service, err = jobs.NewRunnerService(k6, index); err != nil {
			log.Error(err, "Failed to generate k6 test service")
			return err
		}

		if err = ctrl.SetControllerReference(k6, service, r.Scheme); err != nil {
			log.Error(err, "Failed to set controller reference for service")
			return err
		}

		if err = r.Create(ctx, service); err != nil {
			log.Error(err, "Failed to launch k6 test services")
			return err
		}
	}

	return nil
}
