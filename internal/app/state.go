package app

import (
	"github.com/ddmww/grok2api-go/internal/control/account"
	"github.com/ddmww/grok2api-go/internal/control/proxy"
	"github.com/ddmww/grok2api-go/internal/control/refresh"
	"github.com/ddmww/grok2api-go/internal/dataplane/xai"
	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/ddmww/grok2api-go/internal/platform/logstream"
	"github.com/ddmww/grok2api-go/internal/platform/tasks"
)

type State struct {
	Config         *config.Service
	Repo           account.Repository
	Runtime        *account.Runtime
	Refresh        *refresh.Service
	Proxy          *proxy.Runtime
	ProxyClearance *proxy.ClearanceScheduler
	XAI            *xai.Client
	Tasks          *tasks.Store
	Logs           *logstream.Store
}
