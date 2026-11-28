package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

const pinnedTools = "ghcr.io/acme/tools@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTemplateService() *app.Service {
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetTemplateRegistry(store, nil)
	return svc
}

func TestCreateTakesTheNameOfARegisteredTemplate(t *testing.T) {
	svc, ctx := newTemplateService(), context.Background()
	if _, err := svc.RegisterTemplate(ctx, actorA, "tools", pinnedTools, "", false); err != nil {
		t.Fatalf("register: %v", err)
	}

	sb, err := svc.CreateSandboxFromSpec(ctx, actorA, app.SandboxSpec{Template: "tools"})

	if err != nil || sb.Image != pinnedTools {
		t.Fatalf("create from the name tools = image %q, err %v; want %s", imageOf(sb), err, pinnedTools)
	}
}

func TestCreateTakesTheNameOfABuiltInTemplate(t *testing.T) {
	svc := newTemplateService()

	sb, err := svc.CreateSandboxFromSpec(context.Background(), actorA, app.SandboxSpec{Template: "python"})

	if err != nil || !strings.HasPrefix(sb.Image, "python:") {
		t.Fatalf("create from the name python = image %q, err %v; want the built-in python image", imageOf(sb), err)
	}
}

func TestATemplateNameOfAnotherTenantIsNotFound(t *testing.T) {
	svc, ctx := newTemplateService(), context.Background()
	if _, err := svc.RegisterTemplate(ctx, actorA, "tools", pinnedTools, "", false); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := svc.CreateSandboxFromSpec(ctx, actorB, app.SandboxSpec{Template: "tools"})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("tenant B creating from tenant A's template name: err = %v, want ErrNotFound", err)
	}
}

func imageOf(sb *domain.Sandbox) string {
	if sb == nil {
		return ""
	}
	return sb.Image
}
