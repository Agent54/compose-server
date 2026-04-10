package serve

import (
	"context"

	composeapi "github.com/docker/compose/v5/pkg/api"
)

type noopEventProcessor struct{}

func (noopEventProcessor) Start(context.Context, string) {}

func (noopEventProcessor) On(...composeapi.Resource) {}

func (noopEventProcessor) Done(string, bool) {}
