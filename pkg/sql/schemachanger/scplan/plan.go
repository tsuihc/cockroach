// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scplan

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/opgen"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules/current"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules/release_22_2"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/scgraph"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/scstage"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// Params holds the arguments for planning.
type Params struct {
	// ActiveVersion contains the version currently active in the cluster.
	ActiveVersion clusterversion.ClusterVersion

	// InRollback is used to indicate whether we've already been reverted.
	// Note that when in rollback, there is no turning back and all work is
	// non-revertible. Theory dictates that this is fine because of how we
	// had carefully crafted stages to only allow entering rollback while it
	// remains safe to do so.
	InRollback bool

	// ExecutionPhase indicates the phase that the plan should be constructed for.
	ExecutionPhase scop.Phase

	// SchemaChangerJobIDSupplier is used to return the JobID for a
	// job if one should exist.
	SchemaChangerJobIDSupplier func() jobspb.JobID

	// SkipPlannerSanityChecks, if false, strictly enforces sanity checks in the
	// declarative schema changer planner.
	SkipPlannerSanityChecks bool
}

// Exported internal types
type (
	// Graph is an exported alias of scgraph.Graph.
	Graph = scgraph.Graph

	// Stage is an exported alias of scstage.Stage.
	Stage = scstage.Stage
)

// A Plan is a schema change plan, primarily containing ops to be executed that
// are partitioned into stages.
type Plan struct {
	scpb.CurrentState
	Params Params
	Graph  *scgraph.Graph
	JobID  jobspb.JobID
	Stages []Stage
}

// StagesForCurrentPhase returns the stages in the execution phase specified in
// the plan params.
func (p Plan) StagesForCurrentPhase() []scstage.Stage {
	for i, s := range p.Stages {
		if s.Phase > p.Params.ExecutionPhase {
			return p.Stages[:i]
		}
	}
	return p.Stages
}

// MakePlan generates a Plan for a particular phase of a schema change, given
// the initial state for a set of targets. Returns an error when planning fails.
func MakePlan(ctx context.Context, initial scpb.CurrentState, params Params) (p Plan, err error) {
	defer scerrors.StartEventf(
		ctx,
		"building declarative schema changer plan in %s (rollback=%v) for %s",
		redact.Safe(params.ExecutionPhase),
		redact.Safe(params.InRollback),
		redact.Safe(initial.StatementTags()),
	).HandlePanicAndLogError(ctx, &err)
	p = Plan{
		CurrentState: initial,
		Params:       params,
	}
	err = makePlan(ctx, &p)
	if err != nil && ctx.Err() == nil {
		err = p.DecorateErrorWithPlanDetails(err)
	}
	return p, err
}

func makePlan(ctx context.Context, p *Plan) (err error) {
	{
		start := timeutil.Now()
		// Generate the graph used to build the stages.
		//
		// For each element in the target set, the graph needs to cover all the
		// statuses which may be visited by the remainder of the schema change.
		// Most of the time, these transitions follow the directions of the
		// op-edges in the graph (i.e. from top to bottom in the corresponding
		// definition in the opgen package) with the notable exception of the first
		// stage of the pre-commit phase, denoted as the "reset stage". In this
		// stage, the element's status transitions in the opposite direction all
		// the way back to the initial status. For this reason, any plan which
		// might include this stage needs the full graph, not just the subset which
		// covers the statuses from current to target.
		oldCurrent := p.CurrentState.Current
		if p.Params.ExecutionPhase <= scop.PreCommitPhase {
			// We need the full graph at this point because the plan may transition
			// back to the initial statuses at pre-commit time.
			//
			// Generate the full graph using the existing scpb.TargetState.
			// This is necessary because scgraph.Graph queries rely on pointer
			// equality. Instead, temporarily swap out the current statuses.
			p.CurrentState.Current = make([]scpb.Status, len(p.CurrentState.Current))
			for i, t := range p.Targets {
				p.CurrentState.Current[i] = scpb.AsTargetStatus(t.TargetStatus).InitialStatus()
			}
		}
		p.Graph = buildGraph(ctx, p.Params.ActiveVersion, p.CurrentState)
		// Undo any swapping out of the current statuses.
		p.CurrentState.Current = oldCurrent
		if log.ExpensiveLogEnabled(ctx, 2) {
			log.Infof(ctx, "graph generation took %v", timeutil.Since(start))
		}
	}
	{
		start := timeutil.Now()
		p.Stages = scstage.BuildStages(
			ctx,
			p.CurrentState,
			p.Params.ExecutionPhase,
			p.Graph,
			p.Params.SchemaChangerJobIDSupplier,
			!p.Params.SkipPlannerSanityChecks,
		)
		if log.ExpensiveLogEnabled(ctx, 2) {
			log.Infof(ctx, "stage generation took %v", timeutil.Since(start))
		}
	}
	if n := len(p.Stages); n > 0 && p.Stages[n-1].Phase > scop.PreCommitPhase {
		// Only get the job ID if it's actually been assigned already.
		p.JobID = p.Params.SchemaChangerJobIDSupplier()
	}
	if err := scstage.ValidateStages(p.TargetState, p.Stages, p.Graph); err != nil {
		panic(errors.Wrapf(err, "invalid execution plan"))
	}
	return nil
}

// getRulesRegistryForRelease returns the rules registry based on the current
// active version of cockroach. In a mixed version state, it's possible for the state
// generated by a newer version of cockroach to be incompatible with old versions.
// For example dependent objects or combinations of them in a partially executed,
// plan may reach states where older versions of cockroach may not be able
// to plan further (and vice versa). To eliminate the possibility of these issues, we
// will plan with the set of rules belonging to the currently active version. One
// example of this is the dependency between index name and secondary indexes
// is more relaxed on 23.1 vs 22.2, which can lead to scenarios where the index
// name may become public before the index is public (which was disallowed on older
// versions).
func getRulesRegistryForRelease(
	ctx context.Context, activeVersion clusterversion.ClusterVersion,
) *rules.Registry {
	if activeVersion.IsActive(clusterversion.V23_1) {
		return current.GetRegistry()
	} else if activeVersion.IsActive(clusterversion.V22_2) {
		return release_22_2.GetRegistry()
	} else {
		log.Warningf(ctx, "Falling back to the oldest supported version 22.2")
		return release_22_2.GetRegistry()
	}
}

func applyOpRules(
	ctx context.Context, activeVersion clusterversion.ClusterVersion, g *scgraph.Graph,
) (*scgraph.Graph, error) {
	registry := getRulesRegistryForRelease(ctx, activeVersion)
	return registry.ApplyOpRules(ctx, g)
}

func applyDepRules(
	ctx context.Context, activeVersion clusterversion.ClusterVersion, g *scgraph.Graph,
) error {
	registry := getRulesRegistryForRelease(ctx, activeVersion)
	return registry.ApplyDepRules(ctx, g)
}

func buildGraph(
	ctx context.Context, activeVersion clusterversion.ClusterVersion, cs scpb.CurrentState,
) *scgraph.Graph {
	g, err := opgen.BuildGraph(ctx, activeVersion, cs)
	if err != nil {
		panic(errors.Wrapf(err, "build graph op edges"))
	}
	err = applyDepRules(ctx, activeVersion, g)
	if err != nil {
		panic(errors.Wrapf(err, "build graph dep edges"))
	}
	err = g.Validate()
	if err != nil {
		panic(errors.Wrapf(err, "validate graph"))
	}
	g, err = applyOpRules(ctx, activeVersion, g)
	if err != nil {
		panic(errors.Wrapf(err, "mark op edges as no-op"))
	}
	return g
}
