package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runRecoverCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("recover", stderr)
	confirm := fs.Bool("confirm", false, "start or resume the reviewed archive generation")
	id, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if id == "" {
		return fs.usageError("a full local SESSION_ID is required")
	}
	fail := func(err error) int { terminal.Printf(stderr, "agent-archive: recover: %v\n", err); return 1 }
	home, err := env.readHome()
	if err != nil {
		return fail(err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(errNotSetUp)
	}
	store := state.OpenReadOnly(home)
	if next, recorded, err := store.GenerationSuccessor(id); err != nil {
		return fail(err)
	} else if recorded {
		if *confirm {
			unlock, err := lockCollector(home, "recover", env.now())
			if err != nil {
				return fail(err)
			}
			defer unlock()
			store, err = state.Open(home)
			if err != nil {
				return fail(err)
			}
			if err := store.ResumeGenerationRecoveries(context.Background()); err != nil {
				return fail(err)
			}
		}
		terminal.Printf(stdout, "Generation %s already links to %s. Repeating recovery preserves that identity and its original capture time.\n", id, next)
		return 0
	}
	preview, err := loadGenerationPreview(store, cfg, id, env)
	if err != nil {
		return fail(err)
	}
	reg, at, builder := preview.reg, preview.at, preview.build
	terminal.Printf(stdout, "Session %s cannot prove that the current transcript extends its retained history.\n", id)
	terminal.Printf(stdout, "Recovery preserves its archived history, feedback and handoffs under %s, freezes further native capture there, and starts a new linked archive ID from the current filtered transcript. Missing earlier records remain a capture gap.\n", id)
	terminal.Println(stdout, "Each generation expires independently under normal retention. Existing subagents keep their original identities and parent; new subagents use the active generation. Imported generations remain in their original undo batch.")
	terminal.Println(stdout, "The new generation is queued locally for normal sync. Recovery permanently requires a generation-aware writer; older binaries will refuse this data directory.")
	if !*confirm {
		terminal.Printf(stdout, "To start this generation, run agent-archive recover %s --confirm.\n", id)
		return 0
	}
	next, err := confirmGenerationRecovery(home, reg, cfg, at, builder)
	if err != nil {
		return fail(err)
	}
	terminal.Printf(stdout, "Queued new generation %s, linked to %s. Future hooks route to the new ID. Run agent-archive sync to publish it.\n", next, id)
	return 0
}

type generationPreview struct {
	reg   archive.SessionRegistration
	at    time.Time
	build func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error)
}

func loadGenerationPreview(store *state.Store, cfg config.Config, id string, env Env) (generationPreview, error) {
	reg, found, err := store.LoadRegistration(id)
	if err != nil {
		return generationPreview{}, err
	}
	if !found {
		return generationPreview{}, errors.New("session is not registered on this machine; use its full local archive ID")
	}
	if !cfg.AcceptSession(reg) {
		return generationPreview{}, errors.New("session is outside current capture permission or storage destination; review setup first")
	}
	summary, found, err := store.LoadPublishedSummary(id)
	if err != nil {
		return generationPreview{}, err
	}
	if !found || !summary.Published || summary.BlockedReason != state.BlockedReasonTranscriptRewritten {
		return generationPreview{}, errors.New("session has no published transcript_rewritten block")
	}
	if reg.CaptureFrozen {
		return generationPreview{}, errors.New("session is a frozen earlier generation; select its active successor")
	}
	if outstanding, err := store.Outstanding(reg, false); err != nil {
		return generationPreview{}, err
	} else if outstanding.Upload {
		return generationPreview{}, errors.New("settle pending publication with agent-archive sync before recovery")
	}
	opts := collector.Options{Sources: registryFor(env), Parsers: parsersFor(env), MachineID: cfg.MachineID, SkillEvidence: cfg.EffectiveSkillEvidence(), RequireSkillUse: cfg.RequireSkillUse, RepoKey: env.repoKey}
	at := env.now().UTC()
	builder, err := collector.PrepareGenerationRecovery(context.Background(), reg, at, opts)
	if err != nil {
		return generationPreview{}, err
	}
	return generationPreview{reg: reg, at: at, build: builder}, nil
}

func confirmGenerationRecovery(home string, reg archive.SessionRegistration, cfg config.Config, at time.Time, builder func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error)) (string, error) {
	if cfg.Paused {
		return "", errPaused
	}
	unlock, err := lockCollector(home, "recover", at)
	if err != nil {
		return "", err
	}
	defer unlock()
	store, err := state.Open(home)
	if err != nil {
		return "", err
	}
	// Fence downgrade writers before committing any generation transition. Setup
	// writes use hooks.lock too; revalidation inside the builder closes the gap
	// between this fence and state.BeginGenerationRecovery's own short hold.
	hooks, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return "", err
	}
	current, found, err := config.Load(home)
	if err == nil && (!found || current.Paused || !current.AcceptSession(reg)) {
		err = errors.New("capture permission changed; preview recovery again")
	}
	if err == nil {
		current.GenerationProtection = true
		err = config.Save(home, current)
	}
	hooks()
	if err != nil {
		return "", err
	}
	protectedBuilder := func(latest archive.SessionRegistration, id string) (archive.SessionRegistration, state.PendingPublication, error) {
		current, found, err := config.Load(home)
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		if !found || !current.GenerationProtection || current.Paused || !current.AcceptSession(latest) || current.EffectiveSkillEvidence() != cfg.EffectiveSkillEvidence() || current.RequireSkillUse != cfg.RequireSkillUse {
			return latest, state.PendingPublication{}, errors.New("capture policy changed; preview recovery again")
		}
		return builder(latest, id)
	}
	next, err := store.BeginGenerationRecovery(reg.ArchiveSessionID, at, protectedBuilder)
	if err != nil {
		return "", err
	}
	return next, nil
}
