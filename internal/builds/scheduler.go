// Package builds runs image builds: it starts queued builds in hidden builder VMs, renders their env and ends them.
package builds

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/version"
)

const (
	Timeout     = 60 * time.Minute
	BuilderRepo = "ghcr.io/asterism-dev/observatory-builder"

	builderCPUs     = 2
	builderMemoryMB = 4096
	builderDiskGB   = 40
	latestTag       = "latest"
)

// errNameTaken tells a VM name already in the org apart from a build that is no longer queued.
var errNameTaken = errors.New("a VM with the builder's name exists")

type Scheduler struct {
	store      *store.Store
	hubURL     string
	builder    string
	builderErr error
	notify     func(org uuid.UUID)
	online     func(node uuid.UUID) bool
	vmDeleted  func(vm uuid.UUID)
	kick       chan struct{}

	Now func() time.Time
}

// New takes the builder image as repo@digest; builderErr says why it is empty, and every build then fails with it.
// vmDeleted runs for every builder VM the scheduler deletes, to drop what the hub keeps about it in memory.
func New(s *store.Store, hubURL, builder string, builderErr error, notify func(org uuid.UUID), online func(node uuid.UUID) bool, vmDeleted func(vm uuid.UUID)) *Scheduler {
	return &Scheduler{store: s, hubURL: strings.TrimRight(hubURL, "/"), builder: builder, builderErr: builderErr,
		notify: notify, online: online, vmDeleted: vmDeleted, kick: make(chan struct{}, 1), Now: time.Now}
}

// BuilderTag is the builder image tag released with this hub.
func BuilderTag() string {
	if version.Released(version.Version) {
		return version.Version
	}
	return latestTag
}

// ResolveBuilder looks up repo:BuilderTag() anonymously and returns repo@digest, of the linux/amd64 child for an index: that is what the node pulls and reports.
func ResolveBuilder(ctx context.Context, repo string) (string, error) {
	ref, err := name.ParseReference(repo + ":" + BuilderTag())
	if err != nil {
		return "", err
	}
	d, err := registry.PlatformDigest(ctx, ref)
	if err != nil {
		return "", err
	}
	return repo + "@" + d, nil
}

// Kick makes Run look at the queue now.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run ticks every interval and on Kick until ctx ends.
func (s *Scheduler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Error("build scheduler", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// Tick deletes builder VMs no build holds, ends timed-out builds and starts queued ones.
func (s *Scheduler) Tick(ctx context.Context) error {
	orphans, err := s.store.OrphanBuilderVMs(ctx)
	if err != nil {
		return err
	}
	for _, v := range orphans {
		if err := s.store.DeleteVM(ctx, v.OrgID, v.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		s.vmDeleted(v.ID)
		s.notify(v.OrgID)
	}
	active, err := s.store.ActiveBuilds(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range active {
		errs = append(errs, s.step(ctx, b))
	}
	return errors.Join(errs...)
}

func (s *Scheduler) step(ctx context.Context, b store.ImageBuild) error {
	now := s.Now()
	switch {
	case b.State == store.BuildRunning && b.StartedAt != nil && now.Sub(*b.StartedAt) >= Timeout:
		return s.fail(ctx, b.ID, "the build timed out after 60 minutes")
	case b.State == store.BuildQueued && now.Sub(b.QueuedAt) >= Timeout:
		return s.fail(ctx, b.ID, "no node had room for the builder VM within 60 minutes")
	case b.State == store.BuildQueued:
		return s.start(ctx, b)
	}
	return nil
}

func (s *Scheduler) fail(ctx context.Context, id uuid.UUID, msg string) error {
	_, err := s.Finish(ctx, id, store.BuildFailed, "", msg)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	return err
}

func (s *Scheduler) start(ctx context.Context, b store.ImageBuild) error {
	if s.builder == "" {
		return s.fail(ctx, b.ID, fmt.Sprintf("the builder image could not be resolved when the hub started: %v", s.builderErr))
	}
	img, err := s.store.ImageByID(ctx, b.ImageID)
	if err != nil {
		return err
	}
	node, ok, err := s.pickNode(ctx, img.OrgID)
	if err != nil || !ok {
		return err
	}
	_, digest, _ := strings.Cut(s.builder, "@")
	v := store.VM{OrgID: img.OrgID, NodeID: node, Name: "build-" + strings.ReplaceAll(b.ID.String(), "-", ""), Kind: store.VMKindBuilder,
		CPUs: builderCPUs, MemoryMB: builderMemoryMB, DiskGB: builderDiskGB, CreatedBy: b.StartedBy,
		ImageRef: s.builder, ImageDigest: digest, ImageSource: store.VMKindBuilder}
	switch img.Scope {
	case store.EnvScopeTeam:
		v.TeamID = img.OwnerID
	case store.EnvScopeUser:
		v.UserID = img.OwnerID
	default:
		v.UserID = &b.StartedBy
	}
	secret, err := s.store.BuildSecret(ctx, img.OrgID, b.ID)
	if err != nil {
		return err
	}
	base := v.Name
	for try := 0; try < 3; try++ {
		if try > 0 {
			v.Name = base + "-" + strings.ToLower(rand.Text()[:4])
		}
		err = s.store.InTx(ctx, func(tx *store.Store) error {
			created, err := tx.CreateVM(ctx, v)
			if errors.Is(err, store.ErrConflict) {
				return errNameTaken
			} else if err != nil {
				return err
			}
			return tx.StartImageBuild(ctx, b.ID, node, created.ID, secret, s.Now())
		})
		if !errors.Is(err, errNameTaken) {
			break
		}
	}
	switch {
	case errors.Is(err, store.ErrLimitExceeded):
		return s.fail(ctx, b.ID, fmt.Sprintf("the builder VM (%d vCPU, %d MB RAM, %d GB disk) counts against the VM limits: %v", builderCPUs, builderMemoryMB, builderDiskGB, err))
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotEnrolled):
		return s.fail(ctx, b.ID, "the builder VM could not be created: "+err.Error())
	case errors.Is(err, store.ErrConflict):
		return nil // cancelled meanwhile
	case err != nil:
		return err
	}
	s.notify(img.OrgID)
	return nil
}

// pickNode returns the online node of org with the most memory left for the builder VM.
func (s *Scheduler) pickNode(ctx context.Context, org uuid.UUID) (uuid.UUID, bool, error) {
	nodes, err := s.store.Nodes(ctx, org)
	if err != nil {
		return uuid.Nil, false, err
	}
	// ponytail: memory-only fit against the node's reported RAM; add CPU and disk once nodes report usable capacity.
	best, bestFree := uuid.Nil, builderMemoryMB-1
	for _, n := range nodes {
		if !n.Enrolled || n.Quarantined || !s.online(n.ID) {
			continue
		}
		vms, err := s.store.VMsOnNode(ctx, n.ID)
		if err != nil {
			return uuid.Nil, false, err
		}
		free := n.MemoryMB
		for _, v := range vms {
			if v.Power == proto.PowerRunning || v.Power == proto.PowerPaused {
				free -= v.MemoryMB
			}
		}
		if free > bestFree {
			best, bestFree = n.ID, free
		}
	}
	return best, best != uuid.Nil, nil
}

// Finish ends a queued or running build, moves latest to digest on success and deletes its builder VM; ErrConflict when it already ended.
func (s *Scheduler) Finish(ctx context.Context, id uuid.UUID, state, digest, msg string) (store.ImageBuild, error) {
	var b store.ImageBuild
	var org uuid.UUID
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		var err error
		if b, err = tx.FinishImageBuild(ctx, id, state, digest, msg, s.Now()); err != nil {
			return err
		}
		if state == store.BuildSucceeded {
			if err := tx.SetImageTag(ctx, b.ImageID, latestTag, digest); err != nil {
				return err
			}
		}
		if b.Automatic && state != store.BuildCancelled {
			if err := tx.SetImageAutoResult(ctx, b.ImageID, b.Error, s.Now()); err != nil {
				return err
			}
		}
		if err := tx.FinishBuildLog(ctx, id); err != nil {
			return err
		}
		if b.VMID == nil {
			return nil
		}
		img, err := tx.ImageByID(ctx, b.ImageID)
		if err != nil {
			return err
		}
		org = img.OrgID
		if err := tx.DeleteVM(ctx, org, *b.VMID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	})
	if err == nil && org != uuid.Nil {
		s.vmDeleted(*b.VMID)
		s.notify(org)
	}
	return b, err
}

// BootEnv is the whole env of a builder VM: the build settings and the recipe's referenced secrets, plus its referenced integrations as real tokens.
func (s *Scheduler) BootEnv(ctx context.Context, v store.VM) (map[string]string, []proto.IntegrationToken, error) {
	b, err := s.store.RunningBuildOnVM(ctx, v.ID)
	if err != nil {
		return nil, nil, err
	}
	rev, err := s.store.RecipeRevision(ctx, b.ImageID, b.Revision)
	if err != nil {
		return nil, nil, err
	}
	secret, err := s.store.BuildSecret(ctx, v.OrgID, b.ID)
	if err != nil {
		return nil, nil, err
	}
	envs, err := s.store.EnvVars(ctx, v.OrgID)
	if err != nil {
		return nil, nil, err
	}
	igs, err := s.store.Integrations(ctx, v.OrgID)
	if err != nil {
		return nil, nil, err
	}
	env := map[string]string{}
	var secrets, kinds []string
	var tokens []proto.IntegrationToken
	for _, ref := range rev.Refs {
		// A reference deleted since the edit is left unset; the agent then fails the build naming it.
		switch ref.Kind {
		case "secret":
			if !slices.Contains(secrets, ref.Name) {
				secrets = append(secrets, ref.Name)
			}
			i := slices.IndexFunc(envs, func(e store.EnvVar) bool {
				return e.Scope == ref.Scope && sameOwner(e.OwnerID, ref.OwnerID) && e.Name == ref.Name
			})
			if i < 0 {
				continue
			}
			if env[ref.Name], err = s.store.EnvValue(ctx, envs[i]); err != nil {
				return nil, nil, err
			}
		case "integration":
			if !slices.Contains(kinds, ref.Name) {
				kinds = append(kinds, ref.Name)
			}
			i := slices.IndexFunc(igs, func(g store.Integration) bool {
				return g.Scope == ref.Scope && sameOwner(g.OwnerID, ref.OwnerID) && string(g.Kind) == ref.Name
			})
			if i < 0 {
				continue
			}
			tok, err := s.store.IntegrationToken(ctx, igs[i])
			if err != nil {
				return nil, nil, err
			}
			if !igs[i].WasInVM {
				if err := s.store.MarkIntegrationInVM(ctx, igs[i]); err != nil {
					return nil, nil, err
				}
			}
			tokens = append(tokens, proto.IntegrationToken{Kind: ref.Name, Token: tok, ExpiresAt: igs[i].AccessExpiresAt})
		}
	}
	// Set last, so a secret of the same name cannot redirect the agent.
	env["OBSERVATORY_BUILD_URL"] = s.hubURL
	env["OBSERVATORY_BUILD_ID"] = b.ID.String()
	env["OBSERVATORY_BUILD_SECRET"] = secret
	env["OBSERVATORY_BUILD_IMAGE"] = b.ImageID.String()
	env["OBSERVATORY_BUILD_SECRETS"] = strings.Join(secrets, ",")
	env["OBSERVATORY_BUILD_INTEGRATIONS"] = strings.Join(kinds, ",")
	return env, tokens, nil
}

func sameOwner(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
