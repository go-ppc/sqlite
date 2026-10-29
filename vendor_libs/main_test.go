// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build none

package main

import (
	"go/format"
	"reflect"
	"strings"
	"testing"
)

func TestMainEngineTargets(t *testing.T) {
	want := []engineTarget{
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"freebsd", "386"}, {"freebsd", "amd64"}, {"freebsd", "arm"}, {"freebsd", "arm64"},
		{"linux", "386"}, {"linux", "amd64"}, {"linux", "arm"}, {"linux", "arm64"},
		{"linux", "loong64"}, {"linux", "ppc64le"}, {"linux", "riscv64"}, {"linux", "s390x"},
		{"netbsd", "amd64"}, {"openbsd", "amd64"}, {"openbsd", "arm64"},
		{"windows", "386"}, {"windows", "amd64"},
	}
	got, err := engineTargets("")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("default targets = %v, %v; want %v", got, err, want)
	}
	for _, target := range append(want, engineTarget{"linux", "ppc"}, engineTarget{"darwin", "ppc"}, engineTarget{"darwin", "ppc64"}) {
		got, err := engineTargets(target.goos + "/" + target.goarch)
		if err != nil || !reflect.DeepEqual(got, []engineTarget{target}) {
			t.Errorf("single target %v = %v, %v", target, got, err)
		}
	}
	for _, target := range []string{"linux", "linux/", "/ppc", "linux/ppc/amd64", "linux/ppc,darwin/ppc", "plan9/ppc", "linux/ppc64"} {
		if got, err := engineTargets(target); err == nil || got != nil {
			t.Errorf("invalid/unknown target %q accepted: %v, %v", target, got, err)
		}
	}
}

func TestMainTransformEngine(t *testing.T) {
	// Match ccgo's alias declarations and macro-evaluation artifacts. The
	// latter must not cause a second, conflicting unprefixed alias.
	in := []byte(`// Code generated for linux/ppc; DO NOT EDIT.

//go:build linux && ppc

package libsqlite3

const SQLITE_STATIC = 0
const SQLITE_TRANSIENT = 18446744073709551615
const collision = 0
type Tcollision = int32
type Tplain = int64
type Tsqlite3_int64 = int64
func Xexample() {}
`)
	out, err := transformEngine("ccgo_linux_ppc.go", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source(out); err != nil {
		t.Fatalf("invalid Go output: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"package sqlite3", "//go:build linux && ppc", "type plain = Tplain", "type sqlite3_int64 = Tsqlite3_int64", "type Sqlite3_int64 = sqlite3_int64", "func Xexample()", "const collision = 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q", want)
		}
	}
	for _, unwanted := range []string{"package libsqlite3", "const SQLITE_STATIC", "const SQLITE_TRANSIENT", "type collision ="} {
		if strings.Contains(s, unwanted) {
			t.Errorf("output contains %q", unwanted)
		}
	}
}
