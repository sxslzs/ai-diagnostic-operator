package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	diagnosticv1 "github.com/sxslzs/ai-diagnostic-operator/api/v1"
)

// PodWatcherReconciler 监听 Pod 失败事件，自动创建 PodDiagnosis
type PodWatcherReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=diagnostic.sre.example.com,resources=poddiagnoses,verbs=create;get;list;update;patch;delete
// +kubebuilder:rbac:groups=diagnostic.sre.example.com,resources=poddiagnoses/status,verbs=get
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// Reconcile 处理 Pod 对象
func (r *PodWatcherReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 判断 Pod 是否处于失败状态
	if !isPodFailed(&pod) {
		return ctrl.Result{}, nil
	}

	// 检查是否已存在针对该 Pod 的 PodDiagnosis（避免重复创建）
	// 使用字段索引查询 spec.podName（需确保 main.go 中已添加索引）
	diagnosisList := &diagnosticv1.PodDiagnosisList{}
	if err := r.List(ctx, diagnosisList,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{"spec.podName": pod.Name}); err != nil {
		logger.Error(err, "无法查询现有的 PodDiagnosis")
		return ctrl.Result{}, err
	}

	// 如果已有未完成（非终态）的诊断，则跳过
	for _, d := range diagnosisList.Items {
		if d.Status.Phase != "Completed" && d.Status.Phase != "Failed" {
			logger.Info("该 Pod 已有正在进行的诊断，跳过", "diagnosis", d.Name)
			return ctrl.Result{}, nil
		}
	}

	// 创建新的 PodDiagnosis
	diagnosis := &diagnosticv1.PodDiagnosis{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-diagnosis-", pod.Name),
			Namespace:    pod.Namespace,
			Labels: map[string]string{
				"diagnosed-pod": pod.Name,
				"created-by":    "pod-watcher",
			},
		},
		Spec: diagnosticv1.PodDiagnosisSpec{
			PodName:       pod.Name,
			Namespace:     pod.Namespace,
			TriggerReason: fmt.Sprintf("Pod entered failed state: %s", getFailureReason(&pod)),
			TailLines:     100, // 默认行数，可配置
		},
	}

	// 设置 OwnerReference，当 Pod 删除时自动清理诊断
	if err := controllerutil.SetControllerReference(&pod, diagnosis, r.Scheme); err != nil {
		logger.Error(err, "无法设置 OwnerReference")
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, diagnosis); err != nil {
		logger.Error(err, "无法创建 PodDiagnosis")
		return ctrl.Result{}, err
	}

	logger.Info("成功为失败 Pod 创建诊断任务", "pod", pod.Name, "diagnosis", diagnosis.Name)
	return ctrl.Result{}, nil
}

// SetupWithManager 设置控制器，并添加事件过滤器
func (r *PodWatcherReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				pod, ok := e.Object.(*corev1.Pod)
				return ok && isPodFailed(pod)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldPod, okOld := e.ObjectOld.(*corev1.Pod)
				newPod, okNew := e.ObjectNew.(*corev1.Pod)
				if !okOld || !okNew {
					return false
				}
				// 仅当从非失败状态变为失败状态时触发
				return !isPodFailed(oldPod) && isPodFailed(newPod)
			},
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Complete(r)
}

func isPodFailed(pod *corev1.Pod) bool {
    // 1. Phase 为 Failed
    if pod.Status.Phase == corev1.PodFailed {
        return true
    }
    // 2. Phase 为 Pending 且存在调度失败或镜像拉取失败
    if pod.Status.Phase == corev1.PodPending {
        // 检查调度失败
        for _, cond := range pod.Status.Conditions {
            if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
                return true
            }
        }
        // 检查容器等待状态中的错误
        for _, status := range pod.Status.ContainerStatuses {
            if status.State.Waiting != nil {
                reason := status.State.Waiting.Reason
                switch reason {
                case "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff", "CreateContainerError":
                    return true
                }
            }
        }
    }
    // 3. 所有容器已终止且至少一个退出码非零（CrashLoopBackOff 时可能所有容器已终止）
    allTerminated := true
    hasNonZero := false
    for _, status := range pod.Status.ContainerStatuses {
        if status.State.Terminated == nil {
            allTerminated = false
            // 如果容器在等待状态且是错误原因，也视为失败
            if status.State.Waiting != nil {
                reason := status.State.Waiting.Reason
                if reason == "CrashLoopBackOff" || reason == "CreateContainerError" {
                    return true
                }
            }
        } else if status.State.Terminated.ExitCode != 0 {
            hasNonZero = true
        }
    }
    return allTerminated && hasNonZero
}
// getFailureReason 提取简要失败原因
func getFailureReason(pod *corev1.Pod) string {
	if pod.Status.Phase == corev1.PodFailed {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReasonUnschedulable && cond.Status == corev1.ConditionTrue {
				return "Unschedulable: " + cond.Message
			}
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return fmt.Sprintf("Container %s exited with code %d: %s",
				status.Name, status.State.Terminated.ExitCode, status.State.Terminated.Reason)
		}
	}
	return "unknown failure"
}
