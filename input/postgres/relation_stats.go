package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pganalyze/collector/state"
)

const relationStatsSQLInsertsSinceVacuumFieldPg13 string = "pg_stat_get_ins_since_vacuum(c.oid)"
const relationStatsSQLInsertsSinceVacuumFieldDefault string = "0"

const relationStatsSQL = `
WITH locked_relids AS (
	SELECT DISTINCT relation relid FROM pg_catalog.pg_locks WHERE mode = 'AccessExclusiveLock' AND relation IS NOT NULL AND locktype = 'relation'
),
locked_relids_with_parents AS (
	SELECT DISTINCT inhparent relid FROM pg_catalog.pg_inherits WHERE inhrelid IN (SELECT relid FROM locked_relids)
	UNION SELECT relid FROM locked_relids
)
SELECT c.oid,
			 COALESCE(pg_catalog.pg_table_size(c.oid), 0) +
			 COALESCE((
				SELECT pg_catalog.sum(pg_catalog.pg_table_size(inhrelid))
				FROM pg_catalog.pg_inherits
				JOIN pg_class t ON inhrelid = t.oid
				WHERE inhparent = c.oid
					-- Only include sizes from child partitions skipped by ignore_schema_regexp.
					-- Child partitions tracked by the collector have their sizes added later.
					AND ($1 != '' AND (n.nspname || '.' || t.relname) ~* $1)
			 ), 0) AS size_bytes,
			 CASE c.reltoastrelid WHEN NULL THEN 0 ELSE COALESCE(pg_catalog.pg_total_relation_size(c.reltoastrelid), 0) END AS toast_bytes,
			 COALESCE(pg_stat_get_numscans(c.oid), 0) AS seq_scan,
			 COALESCE(pg_stat_get_tuples_returned(c.oid), 0) AS seq_tup_read,
			 COALESCE(i.idx_scan, 0) AS idx_scan,
			 COALESCE(i.idx_tup_fetch + pg_stat_get_tuples_fetched(c.oid), 0) AS idx_tup_fetch,
			 COALESCE(pg_stat_get_tuples_inserted(c.oid), 0) AS n_tup_ins,
			 COALESCE(pg_stat_get_tuples_updated(c.oid), 0) AS n_tup_upd,
			 COALESCE(pg_stat_get_tuples_deleted(c.oid), 0) AS n_tup_del,
			 COALESCE(pg_stat_get_tuples_hot_updated(c.oid), 0) AS n_tup_hot_upd,
			 COALESCE(pg_stat_get_live_tuples(c.oid), 0) AS n_live_tup,
			 COALESCE(pg_stat_get_dead_tuples(c.oid), 0) AS n_dead_tup,
			 COALESCE(pg_stat_get_mod_since_analyze(c.oid), 0) AS n_mod_since_analyze,
			 COALESCE(%s, 0) AS n_ins_since_vacuum,
			 pg_stat_get_last_vacuum_time(c.oid) AS last_vacuum,
			 pg_stat_get_last_autovacuum_time(c.oid) AS last_autovacuum,
			 pg_stat_get_last_analyze_time(c.oid) AS last_analyze,
			 pg_stat_get_last_autoanalyze_time(c.oid) AS last_autoanalyze,
			 COALESCE(pg_stat_get_vacuum_count(c.oid), 0) AS vacuum_count,
			 COALESCE(pg_stat_get_autovacuum_count(c.oid), 0) AS autovacuum_count,
			 COALESCE(pg_stat_get_analyze_count(c.oid), 0) AS analyze_count,
			 COALESCE(pg_stat_get_autoanalyze_count(c.oid), 0) AS autoanalyze_count,
			 COALESCE(pg_stat_get_blocks_fetched(c.oid) - pg_stat_get_blocks_hit(c.oid), 0) AS heap_blks_read,
			 COALESCE(pg_stat_get_blocks_hit(c.oid), 0) AS heap_blks_hit,
			 COALESCE(i.idx_blks_read, 0) AS idx_blks_read,
			 COALESCE(i.idx_blks_hit, 0) AS idx_blks_hit,
			 COALESCE(pg_stat_get_blocks_fetched(toast.oid) - pg_stat_get_blocks_hit(toast.oid), 0) AS toast_blks_read,
			 COALESCE(pg_stat_get_blocks_hit(toast.oid), 0) AS toast_blks_hit,
			 COALESCE(x.idx_blks_read, 0) AS tidx_blks_read,
			 COALESCE(x.idx_blks_hit, 0) AS tidx_blks_hit,
			 CASE WHEN c.relfrozenxid <> '0' THEN pg_catalog.age(c.relfrozenxid) ELSE 0 END AS relation_xid_age,
			 CASE WHEN c.relminmxid <> '0' THEN pg_catalog.mxid_age(c.relminmxid) ELSE 0 END AS relation_mxid_age,
			 c.relpages,
			 c.reltuples,
			 c.relallvisible,
			 false AS exclusively_locked,
			 COALESCE(toast.reltuples, -1) AS toast_reltuples,
			 COALESCE(toast.relpages, 0) AS toast_relpages,
			 COALESCE(pg_relation_filenode(c.oid), 0) AS data_filenode,
			 COALESCE(pg_relation_filenode(c.reltoastrelid), 0) AS toast_filenode
	FROM pg_catalog.pg_class c
	LEFT JOIN pg_catalog.pg_namespace n ON (n.oid = c.relnamespace)
	LEFT JOIN pg_catalog.pg_class toast ON (c.reltoastrelid = toast.oid AND toast.relkind = 't')
	LEFT JOIN LATERAL (
		SELECT sum(pg_stat_get_numscans(indexrelid))::bigint AS idx_scan,
			   sum(pg_stat_get_tuples_fetched(indexrelid))::bigint AS idx_tup_fetch,
			   sum(pg_stat_get_blocks_fetched(pg_index.indexrelid) - pg_stat_get_blocks_hit(pg_index.indexrelid))::bigint AS idx_blks_read,
			   sum(pg_stat_get_blocks_hit(pg_index.indexrelid))::bigint AS idx_blks_hit
		  FROM pg_catalog.pg_index
		 WHERE pg_index.indrelid = c.oid) i ON true
	LEFT JOIN LATERAL (
		SELECT sum(pg_stat_get_blocks_fetched(pg_index.indexrelid) - pg_stat_get_blocks_hit(pg_index.indexrelid))::bigint AS idx_blks_read,
			   sum(pg_stat_get_blocks_hit(pg_index.indexrelid))::bigint AS idx_blks_hit
		  FROM pg_catalog.pg_index
		 WHERE pg_index.indrelid = toast.oid) x ON true
 WHERE c.oid NOT IN (SELECT relid FROM locked_relids_with_parents)
       AND c.relkind IN ('r','v','m','p')
			 AND c.relpersistence <> 't'
			 AND c.oid NOT IN (SELECT pd.objid FROM pg_catalog.pg_depend pd WHERE pd.deptype = 'e' AND pd.classid = 'pg_catalog.pg_class'::regclass)
			 AND %s
			 AND ($1 = '' OR (n.nspname || '.' || c.relname) !~* $1)
 UNION ALL
SELECT relid,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   NULL,
	   NULL,
	   NULL,
	   NULL,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   true AS exclusively_locked,
	   0,
	   0,
	   0,
	   0
  FROM locked_relids_with_parents
`

const indexStatsSQL = `
WITH locked_relids AS (SELECT DISTINCT relation relid FROM pg_catalog.pg_locks WHERE mode = 'AccessExclusiveLock' AND relation IS NOT NULL AND locktype = 'relation')
SELECT s.indexrelid,
			 COALESCE(pg_catalog.pg_relation_size(s.indexrelid), 0) AS size_bytes,
			 COALESCE(s.idx_scan, 0),
			 COALESCE(s.idx_tup_read, 0),
			 COALESCE(s.idx_tup_fetch, 0),
			 COALESCE(sio.idx_blks_read, 0),
			 COALESCE(sio.idx_blks_hit, 0),
			 COALESCE(pg_relation_filenode(s.indexrelid), 0),
			 false AS exclusively_locked
	FROM pg_catalog.pg_stat_user_indexes s
			 LEFT JOIN pg_catalog.pg_statio_user_indexes sio USING (indexrelid)
 WHERE s.indexrelid NOT IN (SELECT relid FROM locked_relids)
			 AND ($1 = '' OR (s.schemaname || '.' || s.relname) !~* $1)
UNION ALL
SELECT relid,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   0,
	   true AS exclusively_locked
  FROM locked_relids
`

func GetRelationStats(ctx context.Context, c *Collection, db *sql.DB, currentDatabaseOid state.Oid, ts state.TransientState) (relStats state.PostgresRelationStatsMap, err error) {
	var insertsSinceVacuumField string
	var systemCatalogFilter string

	if c.PostgresVersion.Numeric >= state.PostgresVersion13 {
		insertsSinceVacuumField = relationStatsSQLInsertsSinceVacuumFieldPg13
	} else {
		insertsSinceVacuumField = relationStatsSQLInsertsSinceVacuumFieldDefault
	}

	if c.PostgresVersion.IsEPAS {
		systemCatalogFilter = relationSQLEPASSystemCatalogFilter
	} else {
		systemCatalogFilter = relationSQLdefaultSystemCatalogFilter
	}

	stmt, err := db.PrepareContext(ctx, QueryMarkerSQL+fmt.Sprintf(relationStatsSQL, insertsSinceVacuumField, systemCatalogFilter))
	if err != nil {
		err = fmt.Errorf("RelationStats/Prepare: %s", err)
		return
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx, c.Config.IgnoreSchemaRegexp)
	if err != nil {
		err = fmt.Errorf("RelationStats/Query: %s", err)
		return
	}
	defer rows.Close()

	relStats = make(state.PostgresRelationStatsMap)
	for rows.Next() {
		var oid state.Oid
		var stats state.PostgresRelationStats
		var dataFilenode state.Oid
		var toastFilenode state.Oid

		err = rows.Scan(&oid, &stats.SizeBytes, &stats.ToastSizeBytes,
			&stats.SeqScan, &stats.SeqTupRead,
			&stats.IdxScan, &stats.IdxTupFetch, &stats.NTupIns,
			&stats.NTupUpd, &stats.NTupDel, &stats.NTupHotUpd,
			&stats.NLiveTup, &stats.NDeadTup, &stats.NModSinceAnalyze, &stats.NInsSinceVacuum,
			&stats.LastVacuum, &stats.LastAutovacuum, &stats.LastAnalyze,
			&stats.LastAutoanalyze, &stats.VacuumCount, &stats.AutovacuumCount,
			&stats.AnalyzeCount, &stats.AutoanalyzeCount, &stats.HeapBlksRead,
			&stats.HeapBlksHit, &stats.IdxBlksRead, &stats.IdxBlksHit,
			&stats.ToastBlksRead, &stats.ToastBlksHit, &stats.TidxBlksRead,
			&stats.TidxBlksHit, &stats.FrozenXIDAge, &stats.MinMXIDAge,
			&stats.Relpages, &stats.Reltuples, &stats.Relallvisible,
			&stats.ExclusivelyLocked, &stats.ToastReltuples, &stats.ToastRelpages,
			&dataFilenode, &toastFilenode)
		if err != nil {
			err = fmt.Errorf("RelationStats/Scan: %s", err)
			return
		}

		bufferCache, ok := ts.BufferCache[currentDatabaseOid]
		if ok {
			stats.CachedDataBytes = bufferCache[dataFilenode]
			stats.CachedToastBytes = bufferCache[toastFilenode]
			// Any non-zero values are later summed up in DatabaseStatistic.UntrackedCacheBytes
			bufferCache[dataFilenode] = 0
			bufferCache[toastFilenode] = 0
		}

		relStats[oid] = stats
	}

	if err = rows.Err(); err != nil {
		err = fmt.Errorf("RelationStats/Rows: %s", err)
		return
	}

	relStats, err = handleRelationStatsAux(ctx, c, db, relStats)

	return
}

func GetIndexStats(ctx context.Context, c *Collection, db *sql.DB, currentDatabaseOid state.Oid, ts state.TransientState) (indexStats state.PostgresIndexStatsMap, err error) {
	stmt, err := db.PrepareContext(ctx, QueryMarkerSQL+indexStatsSQL)
	if err != nil {
		err = fmt.Errorf("IndexStats/Prepare: %s", err)
		return
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx, c.Config.IgnoreSchemaRegexp)
	if err != nil {
		err = fmt.Errorf("IndexStats/Query: %s", err)
		return
	}
	defer rows.Close()

	indexStats = make(state.PostgresIndexStatsMap)
	for rows.Next() {
		var oid state.Oid
		var stats state.PostgresIndexStats
		var filenode state.Oid

		err = rows.Scan(&oid, &stats.SizeBytes, &stats.IdxScan, &stats.IdxTupRead,
			&stats.IdxTupFetch, &stats.IdxBlksRead, &stats.IdxBlksHit, &filenode,
			&stats.ExclusivelyLocked)
		if err != nil {
			err = fmt.Errorf("IndexStats/Scan: %s", err)
			return
		}

		bufferCache, ok := ts.BufferCache[currentDatabaseOid]
		if ok {
			stats.CachedBytes = bufferCache[filenode]
			// Any non-zero values are later summed up in DatabaseStatistic.UntrackedCacheBytes
			bufferCache[filenode] = 0
		}

		indexStats[oid] = stats
	}

	if err = rows.Err(); err != nil {
		err = fmt.Errorf("IndexStats/Rows: %s", err)
		return
	}

	indexStats, err = handleIndexStatsAux(ctx, c, db, indexStats)

	return
}
