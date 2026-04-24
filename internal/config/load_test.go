package config

import "testing"

func TestDefaultIncludesSeparateCodeBuildAndLambdaBackends(t *testing.T) {
	cfg := Default()
	pool := cfg.Pools[1]

	codebuildCfg, ok := pool.Backends["codebuild"]
	if !ok {
		t.Fatal("expected lite pool to include codebuild backend")
	}
	if codebuildCfg.SecretRef != "uecb-codebuild" {
		t.Fatalf("expected codebuild secretRef uecb-codebuild, got %q", codebuildCfg.SecretRef)
	}

	lambdaCfg, ok := pool.Backends["lambda"]
	if !ok {
		t.Fatal("expected lite pool to include lambda backend")
	}
	if lambdaCfg.SecretRef != "uecb-lambda" {
		t.Fatalf("expected lambda secretRef uecb-lambda, got %q", lambdaCfg.SecretRef)
	}
}
