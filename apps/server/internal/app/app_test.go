package app

import (
	"testing"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

func TestDependencyGraphIsComplete(t *testing.T) {
	if err := fx.ValidateApp(Options("../../config/example.yaml", "test", zap.NewNop())); err != nil {
		t.Fatal(err)
	}
}
