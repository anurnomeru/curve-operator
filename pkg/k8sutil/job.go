package k8sutil

import (
	"context"
	"fmt"
	"time"

	batch "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RunReplaceableJob runs a Kubernetes job with the intention that the job can be replaced by
// another call to this function with the same job name. For example, if a storage operator is
// restarted/updated before the job can complete, the operator's next run of the job should replace
// the previous job if deleteIfFound is set to true.
func RunReplaceableJob(ctx context.Context, clientset kubernetes.Interface, job *batch.Job, deleteIfFound bool) error {
	// check if the job was already created and what its status is
	existingJob, err := clientset.BatchV1().Jobs(job.Namespace).Get(job.Name, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		logger.Warningf("failed to detect job %s. %+v", job.Name, err)
	} else if err == nil {
		// if the job is still running, and the caller has not asked for deletion,
		// allow it to continue to completion

		if existingJob.Status.Active > 0 && !deleteIfFound {
			logger.Infof("Found previous job %s. Status=%+v", job.Name, existingJob.Status)
			return nil
		}

		// delete the job that already exists from a previous run
		logger.Infof("Removing previous job %s to start a new one", job.Name)
		err := DeleteBatchJob(ctx, clientset, job.Namespace, existingJob.Name, true)
		if err != nil {
			return fmt.Errorf("failed to remove job %s. %+v", job.Name, err)
		}
	}

	// always create new job
	_, err = clientset.BatchV1().Jobs(job.Namespace).Create(job)
	return err
}

// DeleteBatchJob deletes a Kubernetes job.
func DeleteBatchJob(ctx context.Context, clientset kubernetes.Interface, namespace, name string, wait bool) error {
	propagation := metav1.DeletePropagationForeground
	gracePeriod := int64(0)
	options := &metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod, PropagationPolicy: &propagation}
	if err := clientset.BatchV1().Jobs(namespace).Delete(name, options); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to remove previous provisioning job for node %s. %+v", name, err)
	}

	if !wait {
		return nil
	}

	// Retry for the job to be deleted for 90s. A pod can easily take 60s to timeout before
	// deletion so we add some buffer to that time.
	retries := 30
	sleepInterval := 3 * time.Second
	for i := 0; i < retries; i++ {
		_, err := clientset.BatchV1().Jobs(namespace).Get(name, metav1.GetOptions{})
		if err != nil && errors.IsNotFound(err) {
			logger.Infof("batch job %s deleted", name)
			return nil
		}

		logger.Infof("batch job %s still exists", name)
		time.Sleep(sleepInterval)
	}

	logger.Warningf("gave up waiting for batch job %s to be deleted", name)
	return nil
}

// CheckJobStatus go routine to check job status
func CheckJobStatus(ctx context.Context, clientSet kubernetes.Interface, ticker *time.Ticker, chn chan bool, namespace string, jobName string) {
	for {
		select {
		case <-ticker.C:
			logger.Info("time is up")

			job, err := clientSet.BatchV1().Jobs(namespace).Get(jobName, metav1.GetOptions{})
			if err != nil {
				logger.Errorf("failed to get job %s in cluster", jobName)
				chn <- false
				return
			}

			if job.Status.Succeeded > 0 {
				logger.Infof("job %s has successd", job.Name)
				chn <- true
				return
			}
			logger.Infof("job %s is running", job.Name)
		case <-ctx.Done():
			chn <- false
			logger.Error("go routinue exit because check time is more than 5 mins")
			return
		}
	}
}
