package containers

import (
	corev1 "k8s.io/api/core/v1"
)

// NewStartContainer is used to get a template for a new k6 starting curl container.
func NewStartContainer(hostnames []string, image string, imagePullPolicy corev1.PullPolicy, command []string, env []corev1.EnvVar, securityContext corev1.SecurityContext, resources corev1.ResourceRequirements) corev1.Container {

	return corev1.Container{
		Name:            "k6-curl",
		Image:           image,
		ImagePullPolicy: imagePullPolicy,
		Env:             env,
		Resources:       resources,
		SecurityContext: &securityContext,
	}
}
