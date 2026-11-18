// Package guest holds the script VMs run from their cloud-init seed.
package guest

import _ "embed"

// BootScript is the seed's vmm-boot.sh: it applies the hub-managed SSH keys and variables on every boot.
//
//go:embed vmm-boot.sh
var BootScript string

//go:embed vmm-agent.py
var AgentScript string

//go:embed vmm-integrations.py
var IntegrationsScript string

//go:embed AGENTS.md
var ContextFile string

//go:embed vmm-desktop
var DesktopScript string
