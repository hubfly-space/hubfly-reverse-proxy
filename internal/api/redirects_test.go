package api

import (
	"testing"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func TestNormalizeRedirect(t *testing.T) {
	redirect := models.Redirect{
		SourceDomain: " Example.com ",
		TargetDomain: " WWW.Example.com ",
	}
	if err := normalizeRedirect(&redirect); err != nil {
		t.Fatalf("normalizeRedirect returned error: %v", err)
	}
	if redirect.ID != "example.com" {
		t.Fatalf("expected ID to default to source domain, got %q", redirect.ID)
	}
	if redirect.SourceDomain != "example.com" {
		t.Fatalf("expected normalized source domain, got %q", redirect.SourceDomain)
	}
	if redirect.TargetDomain != "www.example.com" {
		t.Fatalf("expected normalized target domain, got %q", redirect.TargetDomain)
	}
}

func TestNormalizeRedirectRejectsSelfRedirect(t *testing.T) {
	redirect := models.Redirect{
		SourceDomain: "example.com",
		TargetDomain: "example.com",
	}
	if err := normalizeRedirect(&redirect); err == nil {
		t.Fatal("expected self redirect validation error")
	}
}
