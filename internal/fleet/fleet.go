// Package fleet tracks connected nodes and pushes each its desired VM list, always rebuilt from Postgres.
package fleet

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/registry"
	"github.com/asterism-dev/observatory/internal/store"
)

const platformImageRef = "observatory/standard"

type Fleet struct {
	store *store.Store
	authz *authz.Service
	net   *network.Hub

	// seq numbers every desired list hub-wide; starting at the clock (or above any recorded release_seq) keeps it increasing across hub restarts.
	seq atomic.Uint64

	ctx     context.Context
	stop    context.CancelFunc
	updates sync.WaitGroup

	mu           sync.Mutex
	conns        map[uuid.UUID]*conn
	diskErrs     map[uuid.UUID]map[string]string // VM → disk ID → error, from the latest status
	metrics      map[uuid.UUID][]MetricSample
	retrying     map[uuid.UUID]bool       // orgs with a pending integrationRetry push
	imageRetries map[uuid.UUID]imageRetry // by node

	runs           map[uuid.UUID]*updateRun // by org: the active or last run
	updateErrs     map[uuid.UUID]string     // node → failure it reported for its latest update request
	updateTimeouts updateTimeouts
	betweenVMReads func() // test hook in vmsBack

	registryHost string
	plainHTTP    bool
	issuer       *registry.Issuer
	builderBoot  BuilderBoot
}

// BuilderBoot returns a builder VM's whole env and its integration tokens.
type BuilderBoot func(ctx context.Context, v store.VM) (map[string]string, []proto.IntegrationToken, error)

func New(ctx context.Context, s *store.Store, z *authz.Service, n *network.Hub) (*Fleet, error) {
	maxSeq, err := s.MaxReleaseSeq(ctx)
	if err != nil {
		return nil, err
	}
	f := &Fleet{store: s, authz: z, net: n, conns: map[uuid.UUID]*conn{}, diskErrs: map[uuid.UUID]map[string]string{}, metrics: map[uuid.UUID][]MetricSample{}, retrying: map[uuid.UUID]bool{}, imageRetries: map[uuid.UUID]imageRetry{},
		runs: map[uuid.UUID]*updateRun{}, updateErrs: map[uuid.UUID]string{}, updateTimeouts: updateTimeouts{back: 15 * time.Minute, vms: 10 * time.Minute, vmErrGrace: 2 * time.Minute, poll: 2 * time.Second}}
	f.ctx, f.stop = context.WithCancel(ctx)
	f.seq.Store(max(uint64(time.Now().UnixNano()), maxSeq+1))
	return f, nil
}

// PullImagesFrom makes nodes pull VM images through the hub's registry proxy at the hub URL nodes reach, with node pull tokens of iss; call it before nodes connect.
func (f *Fleet) PullImagesFrom(hub *url.URL, iss registry.Issuer) {
	f.registryHost, f.plainHTTP, f.issuer = hub.Host, hub.Scheme == "http", &iss
}

// BootBuildersWith makes builder VMs boot with fn's env instead of their owner's; call it before nodes connect.
func (f *Fleet) BootBuildersWith(fn BuilderBoot) { f.builderBoot = fn }

// Close stops update runs and waits for them; they also stop when New's ctx ends.
func (f *Fleet) Close() {
	f.mu.Lock()
	f.stop()
	f.mu.Unlock()
	f.updates.Wait()
}

// DiskErrors returns the VM's attachment errors by disk ID as its node last reported them; callers must not modify it.
func (f *Fleet) DiskErrors(vm uuid.UUID) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diskErrs[vm]
}

// noteDiskErrors keeps the disk errors of st, but only for a VM in c's last desired list.
func (f *Fleet) noteDiskErrors(c *conn, st proto.VMStatus) {
	id, err := uuid.Parse(st.ID)
	errs := map[string]string{}
	for _, d := range st.Disks {
		if d.Error != "" {
			errs[d.Name] = d.Error
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case err != nil || !c.vms[st.ID]:
	case len(errs) == 0:
		delete(f.diskErrs, id)
	default:
		f.diskErrs[id] = errs
	}
}

// dropDiskErrors forgets the disk errors of c's VMs that are not in keep; f.mu must be held.
func (f *Fleet) dropDiskErrors(c *conn, keep map[string]bool) {
	for id := range c.vms {
		if u, err := uuid.Parse(id); err == nil && !keep[id] {
			delete(f.diskErrs, u)
		}
	}
}

func (f *Fleet) Online(node uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[node] != nil
}

// NotifyOrg schedules a fresh desired list for every connected node of the org; it never waits on a node.
func (f *Fleet) NotifyOrg(org uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		if c.org == org {
			c.kick()
		}
	}
}

// retryIntegrations pushes the org again after integrationRetry; retries already pending absorb later calls.
func (f *Fleet) retryIntegrations(org uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retrying[org] {
		return
	}
	f.retrying[org] = true
	time.AfterFunc(integrationRetry, func() {
		f.mu.Lock()
		delete(f.retrying, org)
		f.mu.Unlock()
		f.NotifyOrg(org)
	})
}

type imageRetry struct {
	delay   time.Duration
	pending bool
}

// retryImages pushes to node again with backoff while it has VMs whose image is not prepared yet; a list without any resets the backoff.
func (f *Fleet) retryImages(node uuid.UUID, waiting bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.imageRetries[node]
	switch {
	case !waiting && !r.pending:
		delete(f.imageRetries, node)
		return
	case !waiting:
		r.delay = 0
	case r.pending:
		return
	default:
		r.delay = min(max(r.delay*2, imageRetryMin), imageRetryMax)
		r.pending = true
		time.AfterFunc(r.delay, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			r := f.imageRetries[node]
			r.pending = false
			f.imageRetries[node] = r
			if c := f.conns[node]; c != nil {
				c.kick()
			}
		})
	}
	f.imageRetries[node] = r
}

func (f *Fleet) Disconnect(node uuid.UUID) {
	f.mu.Lock()
	c := f.conns[node]
	delete(f.conns, node)
	if c != nil {
		f.dropDiskErrors(c, nil)
	}
	f.mu.Unlock()
	if c != nil {
		c.sess.Close()
	}
}

// attach makes c the node's live connection, closing the one it replaces.
func (f *Fleet) attach(c *conn) {
	f.mu.Lock()
	old := f.conns[c.node]
	c.since = time.Now()
	f.conns[c.node] = c
	f.mu.Unlock()
	if old != nil {
		old.sess.Close()
	}
}

func (f *Fleet) detach(c *conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[c.node] == c {
		delete(f.conns, c.node)
		f.dropDiskErrors(c, nil)
	}
}

// failSnapshots runs when a node's connection ends: a stopping or restarting node resumes the VMs it paused, so copies made later would not match.
func (f *Fleet) failSnapshots(node uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	orgs, err := f.store.FailNodeSnapshots(ctx, node)
	if err != nil {
		slog.Error("fail snapshots of disconnected node", "node", node, "err", err)
		return
	}
	for _, org := range orgs {
		f.NotifyOrg(org)
	}
}

// SweepSnapshots fails snapshots whose storage copies took longer than timeout, so their nodes resume the VMs; it returns when ctx ends.
func (f *Fleet) SweepSnapshots(ctx context.Context, every, timeout time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		orgs, err := f.store.FailStaleSnapshots(ctx, timeout)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("sweep snapshots", "err", err)
			}
			continue
		}
		for _, org := range orgs {
			f.NotifyOrg(org)
		}
	}
}

// desired builds the node's list and returns the releasing disks it no longer carries.
func (f *Fleet) desired(ctx context.Context, org, node uuid.UUID) ([]proto.DesiredVM, []uuid.UUID, error) {
	// Read first: a detach committed after this read is still in the list below, so its export is never marked by it.
	releasing, err := f.store.ReleasingDisks(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	vms, err := f.store.VMsOnNode(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	atts, err := f.store.NodeAttachments(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	snaps, err := f.store.NodeSnapshots(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	plans, err := f.store.NodeVMPlans(ctx, node)
	if err != nil {
		return nil, nil, err
	}
	nets, err := f.store.Networks(ctx, org)
	if err != nil {
		return nil, nil, err
	}
	rank := make(map[uuid.UUID]int, len(nets))
	for i, n := range nets {
		rank[n.ID] = i
	}
	out := make([]proto.DesiredVM, 0, len(vms))
	waiting := false
	defer func() { f.retryImages(node, waiting) }()
	for _, v := range vms {
		plan := plans[v.ID]
		if plan.Hold {
			continue
		}
		boot, bound, tokens, unresolved, err := f.boot(ctx, org, v)
		if err != nil {
			return nil, nil, err
		}
		if unresolved {
			f.retryIntegrations(org)
		}
		eg, cas, err := f.egress(ctx, org, v, bound)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // only the vms-row lookup returns ErrNotFound here, so the VM was deleted since VMsOnNode
		case err != nil:
			// One VM's CA trouble must not stall the node's list; its placeholders just reach providers unsubstituted.
			slog.Error("egress for vm", "vm", v.ID, "err", err)
		}
		boot.EgressCAs = cas
		nics, err := f.nics(ctx, v, rank)
		if err != nil {
			return nil, nil, err
		}
		img, err := f.desiredImage(ctx, v)
		if err != nil {
			return nil, nil, err
		}
		waiting = waiting || img != nil
		out = append(out, proto.DesiredVM{
			Spec: proto.VMSpec{ID: v.ID.String(), Name: v.Name, CPUs: v.CPUs, MemoryMB: v.MemoryMB, DiskGB: v.DiskGB, NICs: nics, Attachments: atts[v.ID], Image: img,
				NoTemplate: v.Kind == store.VMKindBuilder},
			Power: v.Power, StopMode: v.StopMode, Generation: v.Generation, Provisioned: v.Provisioned, Boot: boot,
			Snapshots: snaps[v.ID], Restore: plan.Restore, Fork: plan.Fork,
			AutoSuspendMinutes: v.AutoSuspendMinutes, WakeSeq: v.WakeSeq, Egress: eg, Integrations: tokens,
			IntegrationsUnresolved: unresolved,
		})
	}
	return out, releasing, nil
}

// desiredImage is set until the node reports the VM's own image (Provisioned flips before the pull): with a fresh pull token, or Gone if the hub cannot locate it.
func (f *Fleet) desiredImage(ctx context.Context, v store.VM) (*proto.ImageRef, error) {
	if v.ImageDigest == "" || v.ImageRef == platformImageRef || v.Provisioned && strings.HasSuffix(v.Image, "@"+v.ImageDigest) {
		return nil, nil
	}
	if v.Kind == store.VMKindBuilder {
		// The builder image is public, so the node pulls it anonymously.
		repo, _, _ := strings.Cut(v.ImageRef, "@")
		return &proto.ImageRef{Repo: repo, Digest: v.ImageDigest}, nil
	}
	gone := &proto.ImageRef{Digest: v.ImageDigest, Gone: true}
	if f.issuer == nil {
		slog.Error("vm image without a registry", "vm", v.ID)
		return gone, nil
	}
	var ver store.ImageVersion
	var err error
	if v.ImageID != nil {
		ver, err = f.store.ImageVersion(ctx, *v.ImageID, v.ImageDigest)
	} else {
		ver, err = f.store.ImageWithVersion(ctx, v.OrgID, v.ImageDigest)
	}
	if errors.Is(err, store.ErrNotFound) {
		// A VM prepared from a since-deleted index version reported its child digest, which no row maps back anymore.
		if v.Provisioned && v.Image != "" {
			return nil, nil
		}
		return gone, nil
	}
	if err != nil {
		return nil, err
	}
	// Of an index the node pulls, caches and reports the amd64 child.
	digest := v.ImageDigest
	if ver.PlatformDigest != "" {
		if v.Provisioned && strings.HasSuffix(v.Image, "@"+ver.PlatformDigest) {
			return nil, nil
		}
		digest = ver.PlatformDigest
	}
	repo := "i/" + ver.ImageID.String()
	ref := &proto.ImageRef{Repo: f.registryHost + "/" + repo, Digest: digest, PlainHTTP: f.plainHTTP}
	if ref.PullToken, err = f.issuer.NodePullToken(v.OrgID, []string{repo}); err != nil {
		slog.Error("node pull token", "vm", v.ID, "err", err)
	}
	return ref, nil
}

// nics lists the management NIC, then network NICs by network name (rank); names and MACs match the gateway's ports.
func (f *Fleet) nics(ctx context.Context, v store.VM, rank map[uuid.UUID]int) ([]proto.NIC, error) {
	if !v.MgmtIP.IsValid() {
		return []proto.NIC{}, nil
	}
	attached, err := f.store.VMNetworks(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	// A network missing from rank was created after the list was read; the next push picks it up.
	attached = slices.DeleteFunc(attached, func(n store.VMNIC) bool { _, ok := rank[n.NetworkID]; return !ok })
	slices.SortFunc(attached, func(a, b store.VMNIC) int { return rank[a.NetworkID] - rank[b.NetworkID] })
	out := []proto.NIC{{Network: "mgmt", MAC: netaddr.MAC(v.MgmtIP), IP: v.MgmtIP.String(), Port: network.MgmtPort(v.ID)}}
	for _, n := range attached {
		out = append(out, proto.NIC{Network: n.NetworkID.String(), MAC: netaddr.MAC(n.IP), IP: n.IP.String(), Port: network.NICPort(v.ID, n.NetworkID)})
	}
	return out, nil
}

// ponytail: keys and env resolved per VM; cache per owner if nodes carry many VMs of one owner.
func (f *Fleet) boot(ctx context.Context, org uuid.UUID, v store.VM) (_ *proto.Boot, _ []proto.HostBoundSecret, _ []proto.IntegrationToken, unresolved bool, _ error) {
	if v.Kind == store.VMKindBuilder {
		return f.bootBuilder(ctx, v)
	}
	var owner string
	var users []uuid.UUID
	switch {
	case v.UserID != nil:
		owner, users = authz.UserSubject(*v.UserID), []uuid.UUID{*v.UserID}
	case v.TeamID != nil:
		owner = authz.TeamSubject(*v.TeamID)
		members, err := f.store.TeamMembers(ctx, org, *v.TeamID)
		if err != nil {
			return nil, nil, nil, false, err
		}
		for _, m := range members {
			users = append(users, m.User.ID)
		}
	}
	keys, err := f.store.SSHKeysOf(ctx, users)
	if err != nil {
		return nil, nil, nil, false, err
	}
	env, bound, err := f.store.ResolveVMEnv(ctx, f.authz.Enforcer(), org, owner)
	if err != nil {
		return nil, nil, nil, false, err
	}
	var tokens []proto.IntegrationToken
	res, err := f.store.ResolveVMIntegrations(ctx, f.authz.Enforcer(), org, owner)
	if err != nil {
		// Like egress, one VM's integration trouble must not stall the node's list.
		slog.Error("integrations for vm", "vm", v.ID, "reason", "resolve failed")
		unresolved = true
	} else {
		var ib []proto.HostBoundSecret
		ib, tokens = IntegrationEntries(res)
		bound = append(bound, ib...)
	}
	if keys == nil {
		keys = []string{}
	}
	return &proto.Boot{SSHPublicKeys: keys, Env: env}, bound, tokens, unresolved, nil
}

// bootBuilder gives a builder VM no SSH keys and nothing of its owner's env; one whose build ended (ErrNotFound) gets an empty env until it is deleted.
func (f *Fleet) bootBuilder(ctx context.Context, v store.VM) (*proto.Boot, []proto.HostBoundSecret, []proto.IntegrationToken, bool, error) {
	boot := &proto.Boot{SSHPublicKeys: []string{}, Env: map[string]string{}}
	if f.builderBoot == nil {
		slog.Error("builder vm without a build scheduler", "vm", v.ID)
		return boot, nil, nil, false, nil
	}
	env, tokens, err := f.builderBoot(ctx, v)
	if errors.Is(err, store.ErrNotFound) {
		return boot, nil, nil, false, nil
	} else if err != nil {
		return nil, nil, nil, false, err
	}
	boot.Env = env
	return boot, nil, tokens, false, nil
}

// IntegrationEntries turns resolved integrations into proxy secrets (proxy mode) and the tokens a VM receives.
func IntegrationEntries(res []store.ResolvedIntegration) (bound []proto.HostBoundSecret, tokens []proto.IntegrationToken) {
	for _, r := range res {
		if r.Mode == integrations.ModeVM {
			tokens = append(tokens, proto.IntegrationToken{Kind: string(r.Kind), Token: r.Token, ExpiresAt: r.AccessExpiresAt})
			continue
		}
		spec := integrations.Kinds[r.Kind]
		b := proto.HostBoundSecret{ID: "integration:" + r.ID.String(), Name: string(r.Kind) + " integration",
			Placeholder: r.Placeholder, Value: r.Token, Hosts: spec.Hosts, Headers: spec.Headers}
		if r.Kind == integrations.Claude && len(r.Accounts) > 0 {
			b.Account = r.Accounts[0].ID.String()
			for _, a := range r.Accounts[1:] {
				b.Alternates = append(b.Alternates, proto.AlternateValue{Account: a.ID.String(), Value: a.Token})
			}
		}
		bound = append(bound, b)
		tokens = append(tokens, proto.IntegrationToken{Kind: string(r.Kind), Token: r.Placeholder})
	}
	return bound, tokens
}

// desiredAccounts adds the accounts of the integration secrets in bound to into, by integration ID.
func desiredAccounts(into map[string]map[string]bool, bound []proto.HostBoundSecret) {
	for _, b := range bound {
		id, ok := strings.CutPrefix(b.ID, "integration:")
		if !ok || b.Account == "" {
			continue
		}
		if into[id] == nil {
			into[id] = map[string]bool{}
		}
		into[id][b.Account] = true
		for _, a := range b.Alternates {
			into[id][a.Account] = true
		}
	}
}

// NoteUsage records the usage the node's proxies saw, but only for accounts in the node's last desired list, as nodes are untrusted.
func (f *Fleet) NoteUsage(ctx context.Context, node store.Node, obs []proto.UsageObservation) {
	f.mu.Lock()
	var accounts map[string]map[string]bool
	if c := f.conns[node.ID]; c != nil {
		accounts = c.accounts
	}
	f.mu.Unlock()
	for _, o := range obs {
		account, err := uuid.Parse(o.Account)
		if err != nil || !accounts[o.Integration][o.Account] {
			// The ids are not logged: they are whatever the node sent.
			slog.Debug("usage report ignored", "node", node.ID)
			continue
		}
		// Nodes are untrusted: a future ObservedAt would freeze the stored usage and a far LimitedUntil would park the account.
		now := time.Now()
		o.ObservedAt = minTime(o.ObservedAt, now)
		if o.LimitedUntil != nil {
			until := minTime(*o.LimitedUntil, now.Add(maxLimit))
			o.LimitedUntil = &until
		}
		f.recordUsage(ctx, store.UsageObservation{Account: account, FiveHourPct: o.FiveHourPct, WeekPct: o.WeekPct,
			FiveHourResetsAt: o.FiveHourResetsAt, WeekResetsAt: o.WeekResetsAt, LimitedUntil: o.LimitedUntil, Source: "proxy", ObservedAt: o.ObservedAt}, now)
	}
}

const maxLimit = 7 * 24 * time.Hour

func minTime(a, b time.Time) time.Time {
	if a.After(b) {
		return b
	}
	return a
}

func (f *Fleet) recordUsage(ctx context.Context, obs store.UsageObservation, now time.Time) {
	sw, err := f.store.RecordUsage(ctx, obs, now)
	if err != nil {
		slog.Error("record usage", "account", obs.Account, "source", obs.Source, "err", err)
		return
	}
	if sw != nil {
		slog.Info("integration account switched", "integration", sw.IntegrationID, "from", sw.From, "to", sw.To, "limitedUntil", sw.LimitedUntil)
		f.NotifyOrg(sw.OrgID)
	}
}

// egress keeps the VM's CAs current for the bound hosts and returns its egress entry plus the public certs by CA ID; nil, nil when nothing is bound.
func (f *Fleet) egress(ctx context.Context, org uuid.UUID, v store.VM, bound []proto.HostBoundSecret) (*proto.Egress, map[string]string, error) {
	// ponytail: one FOR UPDATE tx per VM per push, also to drop stale CAs of unbound VMs; skip unbound VMs without CA rows if pushes get hot.
	var hosts []string
	for _, b := range bound {
		hosts = append(hosts, b.Hosts...)
	}
	cas, err := f.store.EnsureEgressCAs(ctx, org, v.ID, hosts, time.Now())
	if err != nil || len(bound) == 0 {
		return nil, nil, err
	}
	eg := &proto.Egress{Secrets: bound}
	certs := make(map[string]string, len(cas))
	for _, c := range cas {
		eg.CAs = append(eg.CAs, proto.EgressCA{ID: c.ID.String(), CertPEM: c.CertPEM, KeyPEM: c.KeyPEM})
		certs[c.ID.String()] = c.CertPEM
	}
	return eg, certs, nil
}

// SweepEgressCAs pushes fresh lists to orgs with a VM whose newest CA is due for rotation, so the desired build mints it; it returns when ctx ends.
func (f *Fleet) SweepEgressCAs(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		orgs, err := f.store.EgressCAOrgs(ctx, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("sweep egress cas", "err", err)
			}
			continue
		}
		for _, org := range orgs {
			f.NotifyOrg(org)
		}
	}
}

const refreshAhead = 60 * time.Minute

var clock = time.Now

// RefreshIntegrations refreshes Claude sign-ins due within refreshAhead and pushes the new access tokens; failures back off per account from 1 to 15 minutes. It returns when ctx ends.
func (f *Fleet) RefreshIntegrations(ctx context.Context, oauth integrations.ClaudeOAuth, every time.Duration) {
	type backoff struct {
		until time.Time
		wait  time.Duration
	}
	backoffs := map[store.AccountRef]backoff{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := clock()
		refs, err := f.store.DueOAuthRefreshes(ctx, now.Add(refreshAhead))
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("list due integration refreshes", "err", err)
			}
			continue
		}
		maps.DeleteFunc(backoffs, func(r store.AccountRef, _ backoff) bool { return !slices.Contains(refs, r) })
		for _, ref := range refs {
			b := backoffs[ref]
			now := clock()
			if now.Before(b.until) {
				continue
			}
			// The provider call runs inside the row-locking tx; the timeout bounds both.
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ok, err := f.store.RefreshIntegration(rctx, ref, now.Add(refreshAhead), oauth.Refresh)
			cancel()
			switch {
			case err != nil:
				b.wait = min(max(2*b.wait, time.Minute), 15*time.Minute)
				backoffs[ref] = backoff{until: now.Add(b.wait), wait: b.wait}
				slog.Error("integration refresh failed", "id", ref.ID, "err", err)
			case ok:
				delete(backoffs, ref)
				slog.Info("integration refreshed", "id", ref.ID)
				f.NotifyOrg(ref.OrgID)
			}
		}
	}
}

const inactiveEvery = 15

// limitedUntil is the later reset of the windows at 100 % or more, if in the future.
func limitedUntil(u integrations.Usage, now time.Time) *time.Time {
	var until *time.Time
	for _, w := range []struct {
		pct *float64
		at  *time.Time
	}{{u.FiveHourPct, u.FiveHourResetsAt}, {u.WeekPct, u.WeekResetsAt}} {
		if w.pct != nil && *w.pct >= 100 && w.at != nil && w.at.After(now) && (until == nil || w.at.After(*until)) {
			until = w.at
		}
	}
	return until
}

// PollUsage polls Claude usage: active accounts every tick, others every inactiveEvery ticks; failures back off per account from 1 to 15 minutes. It returns when ctx ends.
func (f *Fleet) PollUsage(ctx context.Context, u integrations.ClaudeUsage, every time.Duration) {
	type backoff struct {
		until time.Time
		wait  time.Duration
	}
	backoffs := map[store.AccountRef]backoff{}
	t := time.NewTicker(every)
	defer t.Stop()
	for tick := 0; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if sws, err := f.store.ResumeLimited(ctx, clock()); err != nil {
			if ctx.Err() == nil {
				slog.Error("resume limited accounts", "err", err)
			}
		} else {
			for _, sw := range sws {
				slog.Info("integration account switched", "integration", sw.IntegrationID, "from", sw.From, "to", sw.To, "limitedUntil", sw.LimitedUntil)
				f.NotifyOrg(sw.OrgID)
			}
		}
		accounts, err := f.store.PollableAccounts(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("list pollable accounts", "err", err)
			}
			continue
		}
		maps.DeleteFunc(backoffs, func(r store.AccountRef, _ backoff) bool {
			return !slices.ContainsFunc(accounts, func(a store.PollAccount) bool { return a.AccountRef == r })
		})
		for _, a := range accounts {
			now := clock()
			b := backoffs[a.AccountRef]
			if (!a.Active && tick%inactiveEvery != 0) || now.Before(b.until) {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			usage, err := u.Usage(pctx, a.Token)
			cancel()
			switch {
			case errors.Is(err, integrations.ErrRejected) && a.Source == integrations.SourceSetupToken:
				// Setup tokens may lack the scope the usage endpoint needs.
				if err := f.store.MarkNoUsageData(ctx, a.AccountRef); err != nil {
					slog.Error("mark no usage data", "id", a.ID, "err", err)
				}
			case err != nil:
				b.wait = min(max(2*b.wait, time.Minute), 15*time.Minute)
				backoffs[a.AccountRef] = backoff{until: now.Add(b.wait), wait: b.wait}
				slog.Error("usage poll failed", "id", a.ID, "err", err)
			default:
				delete(backoffs, a.AccountRef)
				f.recordUsage(ctx, store.UsageObservation{Account: a.ID, FiveHourPct: usage.FiveHourPct, WeekPct: usage.WeekPct,
					FiveHourResetsAt: usage.FiveHourResetsAt, WeekResetsAt: usage.WeekResetsAt, LimitedUntil: limitedUntil(usage, now), Source: "poll", ObservedAt: now}, now)
			}
		}
	}
}
