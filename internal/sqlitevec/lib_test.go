// Copyright 2026 Aeneas Rekkas
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package sqlitevec

import (
	"database/sql"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestDeserializeFloat32(t *testing.T) {
	want := []float32{1.5, -2.25, 0, 3e-7}
	blob, err := SerializeFloat32(want)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := DeserializeFloat32(blob, len(want))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, tt := range []struct {
		name string
		blob []byte
		dims int
	}{
		{"too short", blob[:len(blob)-1], len(want)},
		{"too long", append(slices.Clone(blob), 0), len(want)},
		{"wrong dims", blob, len(want) + 1},
		{"nil blob", nil, len(want)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if vec, ok := DeserializeFloat32(tt.blob, tt.dims); ok || vec != nil {
				t.Errorf("got %v, %v; want nil, false", vec, ok)
			}
		})
	}
}

func TestBundledVersion(t *testing.T) {
	if err := Auto(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "v0.1.9" {
		t.Fatalf("vec_version() = %q, want v0.1.9", version)
	}
}
