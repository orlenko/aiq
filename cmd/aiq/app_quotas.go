package main

import (
	"context"
	"errors"
	"os"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/state"
)

// appPoll says only whether the daemon's poll bookkeeping is known; a bridge
// process cannot read it, so it is never known here. Each account's
// observed_at gives its freshness instead.
type appPoll struct {
	Known bool `json:"known"`
}

// appQuotas is this machine's quota view: the Navigator's accounts, rankings
// and model scope (field names and units as in /api/nav/quotas schema 2),
// without other machines. The app shows one section per Herdr machine.
type appQuotas struct {
	Poll       appPoll           `json:"poll"`
	ModelScope []navQuotaScope   `json:"model_scope"`
	Accounts   []navQuotaAccount `json:"accounts"`
	Rankings   []navQuotaRanking `json:"rankings"`
}

// appProductionQuotas reads this machine's state with its config, and never
// constructs the [fleet] SSH poller: the app asks each machine itself.
func appProductionQuotas() *navQuotaDeps {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	local := *cfg
	local.Fleet = config.Fleet{}
	deps := navProductionQuotas(&local, func() daemon.PollStatus { return daemon.PollStatus{} })
	deps.fleet = nil
	return deps
}

func (env appEnv) quotasData(ctx context.Context) appResult {
	if _, err := os.Lstat(env.statePath); os.IsNotExist(err) {
		return appFailure(appNotSetUp)
	}
	if env.quotas == nil {
		return appFailure(appUnavailable)
	}
	deps := *env.quotas
	deps.fleet = nil
	limits := navProductionPolicy().sessions
	body, err := navQuotasBuild(ctx, &deps, env.now(), state.QuotaSnapshotLimits{Rows: limits.arrayEntries, TextBytes: limits.rowBytes})
	if err != nil || body == nil {
		if err == nil {
			err = errors.New("no quota view")
		}
		return appFailure(appErrorFor(ctx, err))
	}
	return appResult{data: appQuotas{ModelScope: body.ModelScope, Accounts: body.Accounts, Rankings: body.Rankings}}
}
