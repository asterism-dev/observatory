package api

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/registry"
)

func TestTagSyncRunsAfterTheEventHandler(t *testing.T) {
	e := newEnv(t)
	alice, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	fm := &fakeManifests{manifests: map[string]registry.Manifest{digestN(1): amd64Manifest()},
		block: make(chan struct{}), started: make(chan struct{}, 1)}
	h := e.eventsHandler(fm)
	if code := pushEvent(t, h, eventsSecret, imageRepo(img), digestN(1), "v1", "user:"+alice.ID.String()); code != 200 {
		t.Fatalf("push: %d", code)
	}
	<-fm.started
	close(fm.block)
	e.api.tags.wait()
	if fm.tag(imageRepo(img), "latest") != digestN(1) {
		t.Fatalf("registry tags %v", fm.tags)
	}
}

func TestTagSyncEndsWithTheDBTags(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	org := e.createOrg(aliceTok, "Acme")
	img := e.createImage(aliceTok, org, `{"name":"a","scope":"org"}`)
	d1 := e.addVersion(img, 1, "latest", "stable", "build-x")
	d2 := e.addVersion(img, 2)
	fm := &fakeManifests{block: make(chan struct{}), started: make(chan struct{}, 1)}
	ts := NewTagSync(e.store, fm)
	ts.Kick(img)
	<-fm.started
	ctx := context.Background()
	// Changes while the first run is blocked in the registry: both must reach it.
	for _, tag := range []string{"stable", "latest"} {
		if err := e.store.SetImageTag(ctx, img, tag, d2); err != nil {
			t.Fatal(err)
		}
		ts.Kick(img)
	}
	close(fm.block)
	ts.wait()
	want := map[string]string{}
	tags, _ := e.store.ImageTags(ctx, img)
	for tag, d := range tags {
		if !strings.HasPrefix(tag, "build-") {
			want[imageRepo(img)+":"+tag] = d
		}
	}
	if !maps.Equal(fm.tags, want) || want[imageRepo(img)+":latest"] != d2 {
		t.Fatalf("registry %v, want %v (d1 %s)", fm.tags, want, d1)
	}
}

func TestTagSyncNilIsANoOp(t *testing.T) {
	if NewTagSync(nil, nil) != nil {
		t.Fatal("tag sync without a registry")
	}
	var ts *TagSync
	ts.Kick(uuid.New())
}
