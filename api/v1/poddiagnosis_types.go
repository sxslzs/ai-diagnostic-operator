package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodDiagnosisSpec 定义了诊断任务的期望状态和目标
type PodDiagnosisSpec struct {
	// 目标 Pod 的名称
	PodName string `json:"podName"`
	// 目标 Pod 所在的命名空间
	Namespace string `json:"namespace"`
	// 触发诊断的原因，例如 "CrashLoopBackOff", "OOMKilled"
	TriggerReason string `json:"triggerReason,omitempty"`
	// 需要向前获取的日志行数，默认可设为 100
	TailLines int32 `json:"tailLines,omitempty"`
}

// PodDiagnosisStatus 定义了诊断任务的实际状态和 AI 诊断结果
type PodDiagnosisStatus struct {
	// 当前诊断阶段：Pending, Diagnosing, Completed, Failed
	Phase string `json:"phase,omitempty"`
	// AI 总结的根本原因 (Root Cause)
	RootCause string `json:"rootCause,omitempty"`
	// AI 给出的修复建议 (Suggestion)
	Suggestion string `json:"suggestion,omitempty"`
	// 诊断完成的时间戳
	DiagnosisTime *metav1.Time `json:"diagnosisTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodDiagnosis 是 poddiagnoses API 的 Schema
type PodDiagnosis struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodDiagnosisSpec   `json:"spec,omitempty"`
	Status PodDiagnosisStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PodDiagnosisList 包含了一组 PodDiagnosis
type PodDiagnosisList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodDiagnosis `json:"items"`
}

// PodDiagnosisReconciler reconciles a PodDiagnosis object

func init() {
	SchemeBuilder.Register(&PodDiagnosis{}, &PodDiagnosisList{})
}
