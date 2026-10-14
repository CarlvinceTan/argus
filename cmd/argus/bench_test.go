package main

import "testing"

func TestBenchHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"polymux", "--help"}} {
		if err := benchCommand(args); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"polymux"}, {"polymux", "--unknown"}, {"polymux", "--polymux", "checkout", "extra"}, {"polymux", "--polymux", "checkout", "--pass-timeout", "0"}, {"polymux", "--polymux", "checkout", "--pass-timeout", "-1s"}} {
		if err := benchCommand(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}
