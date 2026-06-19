package gettokenshooks

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	tempHome, err := os.MkdirTemp("", "gettokenshooks-home-*")
	if err != nil {
		panic(err)
	}
	previousHome, hadHome := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", tempHome); err != nil {
		panic(err)
	}
	code := m.Run()
	if hadHome {
		_ = os.Setenv("HOME", previousHome)
	} else {
		_ = os.Unsetenv("HOME")
	}
	_ = os.RemoveAll(tempHome)
	os.Exit(code)
}
