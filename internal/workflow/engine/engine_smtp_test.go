package engine_test

import (
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/engine"
	"github.com/beelinewf/wf-engine/internal/workflow/executor"
	"github.com/beelinewf/wf-engine/internal/workflow/mail"
	"github.com/beelinewf/wf-engine/internal/workflow/model"
	"github.com/beelinewf/wf-engine/internal/workflow/repository"
	"gorm.io/gorm"
)

func testEngineWithSMTP(t *testing.T, db *gorm.DB, limits model.Limits, execLimits executor.Limits, deps executor.Dependencies) (*engine.Engine, repository.InstanceRepository) {
	t.Helper()
	instances := repository.NewInstanceRepository(db)
	e := engine.NewEngine(instances, repository.NewParallelRepository(db), executor.NewExecutors(execLimits, nil, deps), executor.NewHookRunner(nil), limits, testLoader(db), sysUserID, model.LeanOptions{})
	return e, instances
}

func TestEngineSMTPFailureRoutesToOnFailure(t *testing.T) {
	db := setupEngineDB(t)
	wfID := createWorkflow(t, db, n1,
		nodeJSON(n1, "external_call", "send-mail", "", n3, "mail_res", map[string]any{
			"smtp_config": map[string]any{
				"to":      []string{"{{ missing.path }}"},
				"subject": "hello",
				"text":    "hi",
			},
			"on_failure": map[string]any{
				"next_node":       n2,
				"output_property": "mail_err",
			},
		}),
		nodeJSON(n2, "input", "fallback-input", "", n3, "", map[string]any{
			"channel":         "http",
			"output_property": "manual_fix",
		}),
		nodeJSON(n3, "script", "done", "return 'done';", "", "out", nil),
	)
	instanceID := insertInstance(t, db, wfID, n1, map[string]any{})
	e, _ := testEngineWithSMTP(t, db, model.DefaultLimits(),
		executor.Limits{SMTPFrom: "relay@example.test"},
		executor.Dependencies{SMTPSender: mail.LogSender{}})
	cur := runEngine(t, db, e, instanceID)

	if cur.Status != model.WorkflowWaiting || cur.WaitingReason != model.WaitingReasonInput {
		t.Fatalf("status = %s / %s, want waiting/input (error: %q)", cur.Status, cur.WaitingReason, cur.Error)
	}
	if cur.Error != "" {
		t.Errorf("cur.Error = %q, want empty for handled failure", cur.Error)
	}
	ctxMap := instanceContext(t, db, instanceID)
	mailErr, ok := ctxMap["mail_err"].(map[string]any)
	if !ok {
		t.Fatalf("mail_err not found in context: %v", ctxMap)
	}
	if mailErr["reason"] != "smtp" {
		t.Errorf("reason = %v, want smtp", mailErr["reason"])
	}
	msg, _ := mailErr["message"].(string)
	if !strings.Contains(msg, "render smtp_config.to[0]") {
		t.Errorf("message = %q, want render error", msg)
	}
	if mailErr["result"] != nil {
		t.Errorf("result = %v, want nil for pre-send failure", mailErr["result"])
	}
}

func TestEngineSMTPSuccessWritesResultShape(t *testing.T) {
	db := setupEngineDB(t)
	wfID := createWorkflow(t, db, n1,
		nodeJSON(n1, "external_call", "send-mail", "", n3, "mail_res", map[string]any{
			"smtp_config": map[string]any{
				"to":      []string{"{{ user.email }}"},
				"subject": "Order {{ order.id }} confirmed",
				"text":    "Hi {{ user.name }}.",
			},
		}),
		nodeJSON(n3, "script", "done", "return 'done';", "", "out", nil),
	)
	instanceID := insertInstance(t, db, wfID, n1, map[string]any{
		"user":  map[string]any{"email": "ada@example.test", "name": "Ada"},
		"order": map[string]any{"id": "7"},
	})
	e, _ := testEngineWithSMTP(t, db, model.DefaultLimits(),
		executor.Limits{SMTPFrom: "relay@example.test"},
		executor.Dependencies{SMTPSender: mail.LogSender{}})
	cur := runEngine(t, db, e, instanceID)

	if cur.Status != model.WorkflowFinished {
		t.Fatalf("status = %s, want finished (error %q)", cur.Status, cur.Error)
	}
	ctxMap := instanceContext(t, db, instanceID)
	res, ok := ctxMap["mail_res"].(map[string]any)
	if !ok {
		t.Fatalf("mail_res not found in context: %v", ctxMap)
	}
	for key := range res {
		switch key {
		case "To", "Cc", "Bcc", "From", "Subject":
		default:
			t.Errorf("mail_res has unexpected key %q (body leak?): %v", key, res)
		}
	}
	to, _ := res["To"].([]any)
	if len(to) != 1 || to[0] != "ada@example.test" {
		t.Errorf("To = %v", res["To"])
	}
	if res["From"] != "relay@example.test" {
		t.Errorf("From = %v, want relay default", res["From"])
	}
	if res["Subject"] != "Order 7 confirmed" {
		t.Errorf("Subject = %v", res["Subject"])
	}
}
