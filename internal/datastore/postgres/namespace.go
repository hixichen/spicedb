package postgres

import (
	"context"
	dbsql "database/sql"
	"fmt"

	sq "github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/authzed/spicedb/internal/datastore"
	pb "github.com/authzed/spicedb/pkg/REDACTEDapi/api"
)

const (
	errUnableToWriteConfig  = "unable to write namespace config: %w"
	errUnableToReadConfig   = "unable to read namespace config: %w"
	errUnableToDeleteConfig = "unable to delete namespace config: %w"
)

var (
	namespaceNameKey = attribute.Key("authzed.com/spicedb/namespaceName")
)

var (
	writeNamespace = psql.Insert(tableNamespace).Columns(
		colNamespace,
		colConfig,
		colCreatedTxn,
	)

	readNamespace = psql.Select(colConfig, colCreatedTxn).
			From(tableNamespace).
			Where(sq.Eq{colDeletedTxn: liveDeletedTxnID})

	deleteNamespace = psql.Update(tableNamespace).Where(sq.Eq{colDeletedTxn: liveDeletedTxnID})

	deleteNamespaceTuples = psql.Update(tableTuple).Where(sq.Eq{colDeletedTxn: liveDeletedTxnID})
)

func (pgd *pgDatastore) WriteNamespace(ctx context.Context, newConfig *pb.NamespaceDefinition) (datastore.Revision, error) {
	ctx, span := tracer.Start(ctx, "WriteNamespace")
	defer span.End()

	span.SetAttributes(namespaceNameKey.String(newConfig.Name))

	serialized, err := proto.Marshal(newConfig)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}
	span.AddEvent("Serialized namespace config")

	tx, err := pgd.db.BeginTxx(ctx, nil)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}
	defer tx.Rollback()
	span.AddEvent("DB transaction established")

	newTxnID, err := createNewTransaction(ctx, tx)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}
	span.AddEvent("Model transaction created")

	sql, args, err := writeNamespace.Values(newConfig.Name, serialized, newTxnID).ToSql()
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}

	_, err = tx.ExecContext(separateContextWithTracing(ctx), sql, args...)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}
	span.AddEvent("Namespace config written")

	err = tx.Commit()
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToWriteConfig, err)
	}
	span.AddEvent("Namespace config committed")

	return revisionFromTransaction(newTxnID), nil
}

func (pgd *pgDatastore) ReadNamespace(ctx context.Context, nsName string) (*pb.NamespaceDefinition, datastore.Revision, error) {
	ctx, span := tracer.Start(ctx, "ReadNamespace", trace.WithAttributes(
		attribute.String("name", nsName),
	))
	defer span.End()

	tx, err := pgd.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
	defer tx.Rollback()

	loaded, version, err := loadNamespace(ctx, nsName, tx)
	switch err {
	case datastore.ErrNamespaceNotFound:
		return nil, datastore.NoRevision, err
	case nil:
		return loaded, version, nil
	default:
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
}

func (pgd *pgDatastore) DeleteNamespace(ctx context.Context, nsName string) (datastore.Revision, error) {
	tx, err := pgd.db.BeginTxx(ctx, nil)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}
	defer tx.Rollback()

	_, version, err := loadNamespace(ctx, nsName, tx)
	switch err {
	case datastore.ErrNamespaceNotFound:
		return datastore.NoRevision, err
	case nil:
		break
	default:
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	newTxnID, err := createNewTransaction(ctx, tx)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	delSQL, delArgs, err := deleteNamespace.
		Set(colDeletedTxn, newTxnID).
		Where(sq.Eq{colNamespace: nsName, colCreatedTxn: version}).
		ToSql()
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	_, err = tx.ExecContext(separateContextWithTracing(ctx), delSQL, delArgs...)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	deleteTupleSQL, deleteTupleArgs, err := deleteNamespaceTuples.
		Set(colDeletedTxn, newTxnID).
		Where(sq.Eq{colNamespace: nsName}).
		ToSql()
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	_, err = tx.ExecContext(separateContextWithTracing(ctx), deleteTupleSQL, deleteTupleArgs...)
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	err = tx.Commit()
	if err != nil {
		return datastore.NoRevision, fmt.Errorf(errUnableToDeleteConfig, err)
	}

	return version, nil
}

func loadNamespace(ctx context.Context, namespace string, tx *sqlx.Tx) (*pb.NamespaceDefinition, datastore.Revision, error) {
	ctx, span := tracer.Start(ctx, "loadNamespace")
	defer span.End()

	sql, args, err := readNamespace.Where(sq.Eq{colNamespace: namespace}).ToSql()
	if err != nil {
		return nil, datastore.NoRevision, err
	}

	var config []byte
	var version datastore.Revision
	err = tx.QueryRowxContext(separateContextWithTracing(ctx), sql, args...).Scan(&config, &version)
	if err != nil {
		if err == dbsql.ErrNoRows {
			err = datastore.ErrNamespaceNotFound
		}
		return nil, datastore.NoRevision, err
	}

	loaded := &pb.NamespaceDefinition{}
	err = proto.Unmarshal(config, loaded)
	if err != nil {
		return nil, datastore.NoRevision, err
	}

	return loaded, version, nil
}
