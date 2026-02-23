package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	diagnosticv1 "github.com/sxslzs/ai-diagnostic-operator/api/v1"
)

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=diagnostic.sre.example.com,resources=poddiagnoses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=diagnostic.sre.example.com,resources=poddiagnoses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=diagnostic.sre.example.com,resources=poddiagnoses/finalizers,verbs=update

// PodDiagnosisReconciler reconciles a PodDiagnosis object
type PodDiagnosisReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Clientset *kubernetes.Clientset
	Recorder  record.EventRecorder
}

// 定义API交互的数据结构
type AIRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    float32         `json:"temperature"`
	ResponseFormat *ResponseFormat `json:"responseFormat"`
}
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type ResponseFormat struct {
	Type string `json:"type"`
}
type AIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}
type DiagnosisResult struct {
	RootCause  string `json:"rootCause"`
	Suggestion string `json:"suggestion"`
}

func buildPrompt(podName, triggerReason, logs string) []Message {
	systemPrompt := `你是一个资深的云原生架构师与 Kubernetes SRE 专家。
你的任务是根据提供的 Pod 故障信息和日志，分析崩溃的根本原因，并给出可执行的修复建议。
请严格按照 JSON 格式输出，务必包含以下两个字段：
1. "rootCause": 用一两句话简明扼要地概括根本原因。
2. "suggestion": 给出具体的排查或修复指令（如修改内存限制、检查配置字典等）。
不要输出任何 Markdown 标记符或其他多余的解释性文字。`

	userPrompt := fmt.Sprintf(`
【异常 Pod 名称】: %s
【触发诊断原因】: %s
【尾部日志 (Stdout/Stderr)】:
---
%s
---`, podName, triggerReason, logs)

	return []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// callAIEndpoint 请求大模型并返回解析后的诊断结果
func callAIEndpoint(ctx context.Context, podName, triggerReason, logs string) (*DiagnosisResult, error) {
	apiKey := os.Getenv("AI_API_KEY")
	apiURL := os.Getenv("AI_API_URL")
	modelName := os.Getenv("AI_MODEL")

	if apiKey == "" || apiURL == "" {
		return nil, fmt.Errorf("AI_API_KEY 或 AI_API_URL 未配置")
	}

	reqBody := AIRequest{
		Model:       modelName,
		Messages:    buildPrompt(podName, triggerReason, logs),
		Temperature: 0.2,
		ResponseFormat: &ResponseFormat{
			Type: "json_object",
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 AI 接口失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI 接口返回非 200 状态码: %d", resp.StatusCode)
	}

	var aiResp AIResponse
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return nil, fmt.Errorf("解析 AI 响应失败: %v", err)
	}

	if len(aiResp.Choices) == 0 {
		return nil, fmt.Errorf("AI 响应内容为空")
	}

	content := aiResp.Choices[0].Message.Content

	var result DiagnosisResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("反序列化 DiagnosisResult 失败, AI 返回内容为: %s, error: %v", content, err)
	}

	return &result, nil
}
func (r *PodDiagnosisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var diagnosis diagnosticv1.PodDiagnosis
	if err := r.Get(ctx, req.NamespacedName, &diagnosis); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 如果已经是终态，跳过处理
	if diagnosis.Status.Phase == "Completed" || diagnosis.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	targetPodName := diagnosis.Spec.PodName
	namespace := diagnosis.Spec.Namespace
	tailLines := int64(diagnosis.Spec.TailLines)
	if tailLines == 0 {
		tailLines = 100
	}

	logger.Info("开始获取异常 Pod 日志", "Pod", targetPodName, "Namespace", namespace)
	logs, err := r.getPodLogs(ctx, namespace, targetPodName, tailLines)
	if err != nil {
		logger.Error(err, "获取 Pod 日志失败")
		base := diagnosis.DeepCopy()
		diagnosis.Status.Phase = "Failed"
		diagnosis.Status.RootCause = fmt.Sprintf("获取日志失败: %v", err)
		diagnosis.Status.DiagnosisTime = &metav1.Time{Time: time.Now()}
		if patchErr := r.Status().Patch(ctx, &diagnosis, client.MergeFrom(base)); patchErr != nil {
			logger.Error(patchErr, "更新 Failed 状态失败")
		}
		return ctrl.Result{}, nil
	}

	logger.Info("成功抓取日志，准备进行 AI 诊断", "LogLength", len(logs))
	logger.Info("开始调用 AI 进行故障诊断。。。")

	aiCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	diagnosisResult, err := callAIEndpoint(aiCtx, targetPodName, diagnosis.Spec.TriggerReason, logs)

	if err != nil {
		logger.Error(err, "AI 诊断过程发生错误")
		base := diagnosis.DeepCopy()
		diagnosis.Status.Phase = "Failed"
		diagnosis.Status.RootCause = fmt.Sprintf("诊断失败: %v", err)
		diagnosis.Status.DiagnosisTime = &metav1.Time{Time: time.Now()}
		if patchErr := r.Status().Patch(ctx, &diagnosis, client.MergeFrom(base)); patchErr != nil {
			logger.Error(patchErr, "更新状态为 Failed 时发生错误")
		}
		return ctrl.Result{}, nil
	}

	logger.Info("AI 诊断完成,准备回写结果", "RootCause", diagnosisResult.RootCause)

	// 尝试绑定 Event 到目标 Pod
	var targetPod corev1.Pod
	podKey := client.ObjectKey{Namespace: diagnosis.Spec.Namespace, Name: diagnosis.Spec.PodName}
	if err := r.Get(ctx, podKey, &targetPod); err != nil {
		logger.Info("目标 Pod 已不存在，跳过 Event 绑定", "Pod", diagnosis.Spec.PodName)
	} else {
		r.Recorder.Eventf(&targetPod, corev1.EventTypeWarning, "AIDiagnosisResult",
			"【AI 根因分析】: %s \n【修复建议】: %s", diagnosisResult.RootCause, diagnosisResult.Suggestion)
		logger.Info("成功将诊断结果作为 Event 绑定至目标 Pod")
	}

	// 更新诊断状态为 Completed
	base := diagnosis.DeepCopy()
	diagnosis.Status.Phase = "Completed"
	diagnosis.Status.RootCause = diagnosisResult.RootCause
	diagnosis.Status.Suggestion = diagnosisResult.Suggestion
	diagnosis.Status.DiagnosisTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Patch(ctx, &diagnosis, client.MergeFrom(base)); err != nil {
		logger.Error(err, "更新 PodDiagnosis 状态为 Completed 时发生错误")
		return ctrl.Result{}, err
	}
	logger.Info("PodDiagnosis 状态更新成功！流程结束。")
	return ctrl.Result{}, nil
}

func (r *PodDiagnosisReconciler) getPodLogs(ctx context.Context, namespace, podName string, tailLines int64) (string, error) {
	podLogOpts := corev1.PodLogOptions{
		TailLines: &tailLines,
	}
	req := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &podLogOpts)
	podLogsStream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("打开日志流失败: %v", err)
	}
	defer podLogsStream.Close()
	buf := new(strings.Builder)
	_, err = io.Copy(buf, podLogsStream)
	if err != nil {
		return "", fmt.Errorf("读取日志内容失败: %v", err)
	}
	return buf.String(), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodDiagnosisReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&diagnosticv1.PodDiagnosis{}).
		Named("poddiagnosis").
		Complete(r)
}
