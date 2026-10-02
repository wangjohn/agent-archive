package cli

import "github.com/wangjohn/agent-archive/internal/agentmeta"

var allHarnesses = agentmeta.SetupNames(productionAgents.Catalog())
