package development

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/spicedb/internal/datastore/memdb"
	"github.com/authzed/spicedb/internal/dispatch"
	"github.com/authzed/spicedb/internal/dispatch/graph"
	maingraph "github.com/authzed/spicedb/internal/graph"
	log "github.com/authzed/spicedb/internal/logging"
	datastoremw "github.com/authzed/spicedb/internal/middleware/datastore"
	"github.com/authzed/spicedb/internal/namespace"
	"github.com/authzed/spicedb/internal/sharederrors"
	"github.com/authzed/spicedb/pkg/datastore"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	devinterface "github.com/authzed/spicedb/pkg/proto/developer/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/spiceerrors"
	"github.com/authzed/spicedb/pkg/tuple"
)

// DevContext holds the various helper types for running the developer calls.
type DevContext struct {
	Ctx            context.Context
	Datastore      datastore.Datastore
	Revision       datastore.Revision
	CompiledSchema *compiler.CompiledSchema
	Dispatcher     dispatch.Dispatcher
}

// NewDevContext creates a new DevContext from the specified request context, parsing and populating
// the datastore as needed.
func NewDevContext(ctx context.Context, requestContext *devinterface.RequestContext) (*DevContext, *devinterface.DeveloperErrors, error) {
	ds, err := memdb.NewMemdbDatastore(0, 0*time.Second, memdb.DisableGC)
	if err != nil {
		return nil, nil, err
	}
	ctx = datastoremw.ContextWithDatastore(ctx, ds)

	dctx, devErrs, nerr := newDevContextWithDatastore(ctx, requestContext, ds)
	if nerr != nil || devErrs != nil {
		// If any form of error occurred, immediately close the datastore
		derr := ds.Close()
		if derr != nil {
			return nil, nil, derr
		}

		return dctx, devErrs, nerr
	}

	return dctx, nil, nil
}

func newDevContextWithDatastore(ctx context.Context, requestContext *devinterface.RequestContext, ds datastore.Datastore) (*DevContext, *devinterface.DeveloperErrors, error) {
	// Compile the schema and load its caveats and namespaces into the datastore.
	compiled, devError, err := CompileSchema(requestContext.Schema)
	if err != nil {
		return nil, nil, err
	}

	if devError != nil {
		return nil, &devinterface.DeveloperErrors{InputErrors: []*devinterface.DeveloperError{devError}}, nil
	}

	var inputErrors []*devinterface.DeveloperError
	currentRevision, err := ds.ReadWriteTx(ctx, func(rwt datastore.ReadWriteTransaction) error {
		inputErrors, err = loadCompiled(ctx, compiled, rwt)
		if err != nil || len(inputErrors) > 0 {
			return err
		}
		// Load the test relationships into the datastore.
		inputErrors, err = loadTuples(ctx, requestContext.Relationships, rwt)
		if err != nil || len(inputErrors) > 0 {
			return err
		}

		return nil
	})
	if err != nil || len(inputErrors) > 0 {
		return nil, &devinterface.DeveloperErrors{InputErrors: inputErrors}, err
	}

	// Sanity check: Make sure the request context for the developer is fully valid. We do this after
	// the loading to ensure that any user-created errors are reported as developer errors,
	// rather than internal errors.
	verr := requestContext.Validate()
	if verr != nil {
		return nil, nil, verr
	}

	return &DevContext{
		Ctx:            ctx,
		Datastore:      ds,
		CompiledSchema: compiled,
		Revision:       currentRevision,
		Dispatcher:     graph.NewLocalOnlyDispatcher(10),
	}, nil, nil
}

// Dispose disposes of the DevContext and its underlying datastore.
func (dc *DevContext) Dispose() {
	if dc.Dispatcher == nil {
		return
	}
	if err := dc.Dispatcher.Close(); err != nil {
		log.Ctx(dc.Ctx).Err(err).Msg("error when disposing of dispatcher in devcontext")
	}

	if dc.Datastore == nil {
		return
	}

	if err := dc.Datastore.Close(); err != nil {
		log.Ctx(dc.Ctx).Err(err).Msg("error when disposing of datastore in devcontext")
	}
}

func loadTuples(ctx context.Context, tuples []*core.RelationTuple, rwt datastore.ReadWriteTransaction) ([]*devinterface.DeveloperError, error) {
	devErrors := make([]*devinterface.DeveloperError, 0, len(tuples))
	updates := make([]*core.RelationTupleUpdate, 0, len(tuples))
	for _, tpl := range tuples {
		verr := tpl.Validate()
		if verr != nil {
			devErrors = append(devErrors, &devinterface.DeveloperError{
				Message: verr.Error(),
				Source:  devinterface.DeveloperError_RELATIONSHIP,
				Kind:    devinterface.DeveloperError_PARSE_ERROR,
				Context: tuple.String(tpl),
			})
			continue
		}

		err := validateTupleWrite(ctx, tpl, rwt)
		if err != nil {
			devErr, wireErr := distinguishGraphError(ctx, err, devinterface.DeveloperError_RELATIONSHIP, 0, 0, tuple.String(tpl))
			if devErr != nil {
				devErrors = append(devErrors, devErr)
				continue
			}

			return devErrors, wireErr
		}

		updates = append(updates, tuple.Touch(tpl))
	}

	err := rwt.WriteRelationships(ctx, updates)

	return devErrors, err
}

func loadCompiled(
	ctx context.Context,
	compiled *compiler.CompiledSchema,
	rwt datastore.ReadWriteTransaction,
) ([]*devinterface.DeveloperError, error) {
	errors := make([]*devinterface.DeveloperError, 0, len(compiled.OrderedDefinitions))
	resolver := namespace.ResolverForPredefinedDefinitions(namespace.PredefinedElements{
		Namespaces: compiled.ObjectDefinitions,
		Caveats:    compiled.CaveatDefinitions,
	})

	for _, caveatDef := range compiled.CaveatDefinitions {
		cverr := namespace.ValidateCaveatDefinition(caveatDef)
		if cverr == nil {
			if err := rwt.WriteCaveats(ctx, []*core.CaveatDefinition{caveatDef}); err != nil {
				return errors, err
			}
			continue
		}

		errWithSource, ok := spiceerrors.AsErrorWithSource(cverr)
		if ok {
			errors = append(errors, &devinterface.DeveloperError{
				Message: cverr.Error(),
				Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
				Source:  devinterface.DeveloperError_SCHEMA,
				Context: errWithSource.SourceCodeString,
				Line:    uint32(errWithSource.LineNumber),
				Column:  uint32(errWithSource.ColumnPosition),
			})
		} else {
			errors = append(errors, &devinterface.DeveloperError{
				Message: cverr.Error(),
				Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
				Source:  devinterface.DeveloperError_SCHEMA,
				Context: caveatDef.Name,
			})
		}
	}

	for _, nsDef := range compiled.ObjectDefinitions {
		ts, terr := namespace.NewNamespaceTypeSystem(nsDef, resolver)
		if terr != nil {
			errWithSource, ok := spiceerrors.AsErrorWithSource(terr)
			if ok {
				errors = append(errors, &devinterface.DeveloperError{
					Message: terr.Error(),
					Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
					Source:  devinterface.DeveloperError_SCHEMA,
					Context: errWithSource.SourceCodeString,
					Line:    uint32(errWithSource.LineNumber),
					Column:  uint32(errWithSource.ColumnPosition),
				})
				continue
			}

			errors = append(errors, &devinterface.DeveloperError{
				Message: terr.Error(),
				Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
				Source:  devinterface.DeveloperError_SCHEMA,
				Context: nsDef.Name,
			})
			continue
		}

		_, tverr := ts.Validate(ctx)
		if tverr == nil {
			if err := rwt.WriteNamespaces(ctx, nsDef); err != nil {
				return errors, err
			}
			continue
		}

		errWithSource, ok := spiceerrors.AsErrorWithSource(tverr)
		if ok {
			errors = append(errors, &devinterface.DeveloperError{
				Message: tverr.Error(),
				Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
				Source:  devinterface.DeveloperError_SCHEMA,
				Context: errWithSource.SourceCodeString,
				Line:    uint32(errWithSource.LineNumber),
				Column:  uint32(errWithSource.ColumnPosition),
			})
		} else {
			errors = append(errors, &devinterface.DeveloperError{
				Message: tverr.Error(),
				Kind:    devinterface.DeveloperError_SCHEMA_ISSUE,
				Source:  devinterface.DeveloperError_SCHEMA,
				Context: nsDef.Name,
			})
		}
	}

	return errors, nil
}

// DistinguishGraphError turns an error from a dispatch call into either a user-facing
// DeveloperError or an internal error, based on the error raised by the dispatcher.
func DistinguishGraphError(devContext *DevContext, dispatchError error, source devinterface.DeveloperError_Source, line uint32, column uint32, context string) (*devinterface.DeveloperError, error) {
	return distinguishGraphError(devContext.Ctx, dispatchError, source, line, column, context)
}

func distinguishGraphError(ctx context.Context, dispatchError error, source devinterface.DeveloperError_Source, line uint32, column uint32, context string) (*devinterface.DeveloperError, error) {
	var nsNotFoundError sharederrors.UnknownNamespaceError
	var relNotFoundError sharederrors.UnknownRelationError

	if errors.Is(dispatchError, dispatch.ErrMaxDepth) {
		return &devinterface.DeveloperError{
			Message: dispatchError.Error(),
			Source:  source,
			Kind:    devinterface.DeveloperError_MAXIMUM_RECURSION,
			Line:    line,
			Column:  column,
			Context: context,
		}, nil
	}

	if errors.As(dispatchError, &nsNotFoundError) {
		return &devinterface.DeveloperError{
			Message: dispatchError.Error(),
			Source:  source,
			Kind:    devinterface.DeveloperError_UNKNOWN_OBJECT_TYPE,
			Line:    line,
			Column:  column,
			Context: context,
		}, nil
	}

	if errors.As(dispatchError, &relNotFoundError) {
		return &devinterface.DeveloperError{
			Message: dispatchError.Error(),
			Source:  source,
			Kind:    devinterface.DeveloperError_UNKNOWN_RELATION,
			Line:    line,
			Column:  column,
			Context: context,
		}, nil
	}

	var ire invalidRelationError
	if errors.As(dispatchError, &ire) {
		return &devinterface.DeveloperError{
			Message: dispatchError.Error(),
			Source:  source,
			Kind:    devinterface.DeveloperError_UNKNOWN_RELATION,
			Line:    line,
			Column:  column,
			Context: context,
		}, nil
	}

	return nil, rewriteACLError(ctx, dispatchError)
}

func rewriteACLError(ctx context.Context, err error) error {
	var nsNotFoundError sharederrors.UnknownNamespaceError
	var relNotFoundError sharederrors.UnknownRelationError

	switch {
	case errors.As(err, &nsNotFoundError):
		fallthrough
	case errors.As(err, &relNotFoundError):
		fallthrough

	case errors.As(err, &maingraph.ErrRequestCanceled{}):
		return status.Errorf(codes.Canceled, "request canceled: %s", err)

	case errors.As(err, &maingraph.ErrInvalidArgument{}):
		return status.Errorf(codes.InvalidArgument, "%s", err)

	case errors.As(err, &datastore.ErrInvalidRevision{}):
		return status.Errorf(codes.OutOfRange, "invalid zookie: %s", err)

	case errors.As(err, &maingraph.ErrRelationMissingTypeInfo{}):
		return status.Errorf(codes.FailedPrecondition, "failed precondition: %s", err)

	case errors.As(err, &maingraph.ErrAlwaysFail{}):
		log.Ctx(ctx).Err(err).Msg("internal graph error in devcontext")
		return status.Errorf(codes.Internal, "internal error: %s", err)

	default:
		if errors.As(err, &invalidRelationError{}) {
			return status.Errorf(codes.InvalidArgument, "%s", err)
		}

		log.Ctx(ctx).Err(err).Msg("unexpected graph error in devcontext")
		return err
	}
}
