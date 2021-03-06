// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq" // required for DB access
	"github.com/mainflux/mainflux/pkg/errors"
	"github.com/mainflux/mainflux/things"
)

const (
	errDuplicate  = "unique_violation"
	errFK         = "foreign_key_violation"
	errInvalid    = "invalid_text_representation"
	errTruncation = "string_data_right_truncation"
)

var _ things.ThingRepository = (*thingRepository)(nil)

type thingRepository struct {
	db Database
}

// NewThingRepository instantiates a PostgreSQL implementation of thing
// repository.
func NewThingRepository(db Database) things.ThingRepository {
	return &thingRepository{
		db: db,
	}
}

func (tr thingRepository) Save(ctx context.Context, ths ...things.Thing) ([]things.Thing, error) {
	tx, err := tr.db.BeginTxx(ctx, nil)
	if err != nil {
		return []things.Thing{}, errors.Wrap(things.ErrCreateEntity, err)
	}

	q := `INSERT INTO things (id, owner, name, key, metadata)
		  VALUES (:id, :owner, :name, :key, :metadata);`

	for _, thing := range ths {
		dbth, err := toDBThing(thing)
		if err != nil {
			return []things.Thing{}, errors.Wrap(things.ErrCreateEntity, err)
		}

		if _, err := tx.NamedExecContext(ctx, q, dbth); err != nil {
			tx.Rollback()
			pqErr, ok := err.(*pq.Error)
			if ok {
				switch pqErr.Code.Name() {
				case errInvalid, errTruncation:
					return []things.Thing{}, errors.Wrap(things.ErrMalformedEntity, err)
				case errDuplicate:
					return []things.Thing{}, errors.Wrap(things.ErrConflict, err)
				}
			}

			return []things.Thing{}, errors.Wrap(things.ErrCreateEntity, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return []things.Thing{}, errors.Wrap(things.ErrCreateEntity, err)
	}

	return ths, nil
}

func (tr thingRepository) Update(ctx context.Context, t things.Thing) error {
	q := `UPDATE things SET name = :name, metadata = :metadata WHERE owner = :owner AND id = :id;`

	dbth, err := toDBThing(t)
	if err != nil {
		return errors.Wrap(things.ErrUpdateEntity, err)
	}

	res, errdb := tr.db.NamedExecContext(ctx, q, dbth)
	if errdb != nil {
		pqErr, ok := errdb.(*pq.Error)
		if ok {
			switch pqErr.Code.Name() {
			case errInvalid, errTruncation:
				return errors.Wrap(things.ErrMalformedEntity, errdb)
			}
		}

		return errors.Wrap(things.ErrUpdateEntity, errdb)
	}

	cnt, errdb := res.RowsAffected()
	if err != nil {
		return errors.Wrap(things.ErrUpdateEntity, errdb)
	}

	if cnt == 0 {
		return things.ErrNotFound
	}

	return nil
}

func (tr thingRepository) UpdateKey(ctx context.Context, owner, id, key string) error {
	q := `UPDATE things SET key = :key WHERE owner = :owner AND id = :id;`

	dbth := dbThing{
		ID:    id,
		Owner: owner,
		Key:   key,
	}

	res, err := tr.db.NamedExecContext(ctx, q, dbth)
	if err != nil {
		pqErr, ok := err.(*pq.Error)
		if ok {
			switch pqErr.Code.Name() {
			case errInvalid:
				return errors.Wrap(things.ErrMalformedEntity, err)
			case errDuplicate:
				return errors.Wrap(things.ErrConflict, err)
			}
		}

		return errors.Wrap(things.ErrUpdateEntity, err)
	}

	cnt, err := res.RowsAffected()
	if err != nil {
		return errors.Wrap(things.ErrUpdateEntity, err)
	}

	if cnt == 0 {
		return things.ErrNotFound
	}

	return nil
}

func (tr thingRepository) RetrieveByID(ctx context.Context, owner, id string) (things.Thing, error) {
	q := `SELECT name, key, metadata FROM things WHERE id = $1 AND owner = $2;`

	dbth := dbThing{
		ID:    id,
		Owner: owner,
	}

	if err := tr.db.QueryRowxContext(ctx, q, id, owner).StructScan(&dbth); err != nil {
		pqErr, ok := err.(*pq.Error)
		if err == sql.ErrNoRows || ok && errInvalid == pqErr.Code.Name() {
			return things.Thing{}, errors.Wrap(things.ErrNotFound, err)
		}
		return things.Thing{}, errors.Wrap(things.ErrSelectEntity, err)
	}

	return toThing(dbth)
}

func (tr thingRepository) RetrieveByKey(ctx context.Context, key string) (string, error) {
	q := `SELECT id FROM things WHERE key = $1;`

	var id string
	if err := tr.db.QueryRowxContext(ctx, q, key).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return "", errors.Wrap(things.ErrNotFound, err)
		}
		return "", errors.Wrap(things.ErrSelectEntity, err)
	}

	return id, nil
}

func (tr thingRepository) RetrieveAll(ctx context.Context, owner string, offset, limit uint64, name string, tm things.Metadata) (things.Page, error) {
	nq, name := getNameQuery(name)
	m, mq, err := getMetadataQuery(tm)
	if err != nil {
		return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
	}

	q := fmt.Sprintf(`SELECT id, name, key, metadata FROM things
		  WHERE owner = :owner %s%s ORDER BY id LIMIT :limit OFFSET :offset;`, mq, nq)

	params := map[string]interface{}{
		"owner":    owner,
		"limit":    limit,
		"offset":   offset,
		"name":     name,
		"metadata": m,
	}

	rows, err := tr.db.NamedQueryContext(ctx, q, params)
	if err != nil {
		return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
	}
	defer rows.Close()

	var items []things.Thing
	for rows.Next() {
		dbth := dbThing{Owner: owner}
		if err := rows.StructScan(&dbth); err != nil {
			return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
		}

		th, err := toThing(dbth)
		if err != nil {
			return things.Page{}, errors.Wrap(things.ErrViewEntity, err)
		}

		items = append(items, th)
	}

	cq := fmt.Sprintf(`SELECT COUNT(*) FROM things WHERE owner = :owner %s%s;`, nq, mq)

	total, err := total(ctx, tr.db, cq, params)
	if err != nil {
		return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
	}

	page := things.Page{
		Things: items,
		PageMetadata: things.PageMetadata{
			Total:  total,
			Offset: offset,
			Limit:  limit,
		},
	}

	return page, nil
}

func (tr thingRepository) RetrieveByChannel(ctx context.Context, owner, channel string, offset, limit uint64, connected bool) (things.Page, error) {
	// Verify if UUID format is valid to avoid internal Postgres error
	if _, err := uuid.FromString(channel); err != nil {
		return things.Page{}, things.ErrNotFound
	}

	var q, qc string
	switch connected {
	case true:
		q = `SELECT id, name, key, metadata
		        FROM things th
		        INNER JOIN connections conn
		        ON th.id = conn.thing_id
		        WHERE th.owner = :owner AND conn.channel_id = :channel
		        ORDER BY th.id
		        LIMIT :limit
		        OFFSET :offset;`

		qc = `SELECT COUNT(*)
		        FROM things th
		        INNER JOIN connections conn
		        ON th.id = conn.thing_id
		        WHERE th.owner = $1 AND conn.channel_id = $2;`
	default:
		q = `SELECT id, name, key, metadata
		        FROM things th
		        WHERE th.owner = :owner AND th.id NOT IN
		        (SELECT id FROM things th
		          INNER JOIN connections conn
		          ON th.id = conn.thing_id
		          WHERE th.owner = :owner AND conn.channel_id = :channel)
		        ORDER BY th.id
		        LIMIT :limit
		        OFFSET :offset;`

		qc = `SELECT COUNT(*)
		        FROM things th
		        WHERE th.owner = $1 AND th.id NOT IN
		        (SELECT id FROM things th
		          INNER JOIN connections conn
		          ON th.id = conn.thing_id
		          WHERE th.owner = $1 AND conn.channel_id = $2);`
	}

	params := map[string]interface{}{
		"owner":   owner,
		"channel": channel,
		"limit":   limit,
		"offset":  offset,
	}

	rows, err := tr.db.NamedQueryContext(ctx, q, params)
	if err != nil {
		return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
	}
	defer rows.Close()

	var items []things.Thing
	for rows.Next() {
		dbth := dbThing{Owner: owner}
		if err := rows.StructScan(&dbth); err != nil {
			return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
		}

		th, err := toThing(dbth)
		if err != nil {
			return things.Page{}, errors.Wrap(things.ErrViewEntity, err)
		}

		items = append(items, th)
	}

	var total uint64
	if err := tr.db.GetContext(ctx, &total, qc, owner, channel); err != nil {
		return things.Page{}, errors.Wrap(things.ErrSelectEntity, err)
	}

	return things.Page{
		Things: items,
		PageMetadata: things.PageMetadata{
			Total:  total,
			Offset: offset,
			Limit:  limit,
		},
	}, nil
}

func (tr thingRepository) Remove(ctx context.Context, owner, id string) error {
	dbth := dbThing{
		ID:    id,
		Owner: owner,
	}
	q := `DELETE FROM things WHERE id = :id AND owner = :owner;`
	if _, err := tr.db.NamedExecContext(ctx, q, dbth); err != nil {
		return errors.Wrap(things.ErrRemoveEntity, err)
	}
	return nil
}

type dbThing struct {
	ID       string `db:"id"`
	Owner    string `db:"owner"`
	Name     string `db:"name"`
	Key      string `db:"key"`
	Metadata []byte `db:"metadata"`
}

func toDBThing(th things.Thing) (dbThing, error) {
	data := []byte("{}")
	if len(th.Metadata) > 0 {
		b, err := json.Marshal(th.Metadata)
		if err != nil {
			return dbThing{}, errors.Wrap(things.ErrMalformedEntity, err)
		}
		data = b
	}

	return dbThing{
		ID:       th.ID,
		Owner:    th.Owner,
		Name:     th.Name,
		Key:      th.Key,
		Metadata: data,
	}, nil
}

func toThing(dbth dbThing) (things.Thing, error) {
	var metadata map[string]interface{}
	if err := json.Unmarshal([]byte(dbth.Metadata), &metadata); err != nil {
		return things.Thing{}, errors.Wrap(things.ErrMalformedEntity, err)
	}

	return things.Thing{
		ID:       dbth.ID,
		Owner:    dbth.Owner,
		Name:     dbth.Name,
		Key:      dbth.Key,
		Metadata: metadata,
	}, nil
}
