// Copyright (C) MongoDB, Inc. 2019-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package operation

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo/description"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/mongo/driver/session"
)

// Count represents a count operation.
type Count struct {
	maxTimeMS      *int64
	query          bsoncore.Document
	session        *session.Client
	clock          *session.ClusterClock
	collection     string
	monitor        *event.CommandMonitor
	crypt          driver.Crypt
	database       string
	deployment     driver.Deployment
	readConcern    *readconcern.ReadConcern
	readPreference *readpref.ReadPref
	selector       description.ServerSelector
	retry          *driver.RetryMode
	result         CountResult
	serverAPI      *driver.ServerAPIOptions
}

// CountResult represents a count result returned by the server.
type CountResult struct {
	// The number of documents found
	N int64
}

func buildCountResult(response bsoncore.Document) (CountResult, error) {
	elements, err := response.Elements()
	if err != nil {
		return CountResult{}, err
	}
	cr := CountResult{}
	for _, element := range elements {
		switch element.Key() {
		case "n": // for count using original command
			var ok bool
			cr.N, ok = element.Value().AsInt64OK()
			if !ok {
				return cr, fmt.Errorf("response field 'n' is type int64, but received BSON type %s",
					element.Value().Type)
			}
		case "cursor": // for count using aggregate with $collStats
			firstBatch, err := element.Value().Document().LookupErr("firstBatch")
			if err != nil {
				return cr, err
			}

			// get count value from first batch
			val := firstBatch.Array().Index(0)
			count, err := val.Document().LookupErr("n")
			if err != nil {
				return cr, err
			}

			// use count as Int64 for result
			var ok bool
			cr.N, ok = count.AsInt64OK()
			if !ok {
				return cr, fmt.Errorf("response field 'n' is type int64, but received BSON type %s",
					element.Value().Type)
			}
		}
	}
	return cr, nil
}

// NewCount constructs and returns a new Count.
func NewCount() *Count {
	return &Count{}
}

// Result returns the result of executing this operation.
func (c *Count) Result() CountResult { return c.result }

func (c *Count) processResponse(info driver.ResponseInfo) error {
	var err error
	c.result, err = buildCountResult(info.ServerResponse)
	return err
}

// Execute runs this operations and returns an error if the operaiton did not execute successfully.
func (c *Count) Execute(ctx context.Context) error {
	if c.deployment == nil {
		return errors.New("the Count operation must have a Deployment set before Execute can be called")
	}

	err := driver.Operation{
		CommandFn:         c.command,
		ProcessResponseFn: c.processResponse,
		RetryMode:         c.retry,
		Type:              driver.Read,
		Client:            c.session,
		Clock:             c.clock,
		CommandMonitor:    c.monitor,
		Crypt:             c.crypt,
		Database:          c.database,
		Deployment:        c.deployment,
		ReadConcern:       c.readConcern,
		ReadPreference:    c.readPreference,
		Selector:          c.selector,
		ServerAPI:         c.serverAPI,
	}.Execute(ctx, nil)

	// Swallow error if NamespaceNotFound(26) is returned from aggregate on non-existent namespace
	if err != nil {
		dErr, ok := err.(driver.Error)
		if ok && dErr.Code == 26 {
			err = nil
		}
	}
	return err
}

func (c *Count) command(dst []byte, desc description.SelectedServer) ([]byte, error) {
	switch {
	case desc.WireVersion.Max < 12: // If wire version < 12 (4.9.0), use count command
		dst = bsoncore.AppendStringElement(dst, "count", c.collection)
		if c.query != nil {
			dst = bsoncore.AppendDocumentElement(dst, "query", c.query)
		}
	default: // If wire version >= 12 (4.9.0), use aggregate with $collStats
		dst = bsoncore.AppendStringElement(dst, "aggregate", c.collection)
		var idx int32
		idx, dst = bsoncore.AppendDocumentElementStart(dst, "cursor")
		dst, _ = bsoncore.AppendDocumentEnd(dst, idx)
		if c.query != nil {
			return nil, fmt.Errorf("'query' cannot be set on Count against servers at or above 4.9.0")
		}

		collStatsStage := bsoncore.NewDocumentBuilder().
			AppendDocument("$collStats", bsoncore.NewDocumentBuilder().
				AppendDocument("count", bsoncore.NewDocumentBuilder().Build()).
				Build()).
			Build()
		groupStage := bsoncore.NewDocumentBuilder().
			AppendDocument("$group", bsoncore.NewDocumentBuilder().
				AppendInt64("_id", 1).
				AppendDocument("n", bsoncore.NewDocumentBuilder().
					AppendString("$sum", "$count").Build()).
				Build()).
			Build()
		countPipeline := bsoncore.NewArrayBuilder().
			AppendDocument(collStatsStage).
			AppendDocument(groupStage).
			Build()
		dst = bsoncore.AppendArrayElement(dst, "pipeline", countPipeline)
	}

	if c.maxTimeMS != nil {
		dst = bsoncore.AppendInt64Element(dst, "maxTimeMS", *c.maxTimeMS)
	}
	return dst, nil
}

// MaxTimeMS specifies the maximum amount of time to allow the query to run.
func (c *Count) MaxTimeMS(maxTimeMS int64) *Count {
	if c == nil {
		c = new(Count)
	}

	c.maxTimeMS = &maxTimeMS
	return c
}

// Query determines what results are returned from find.
func (c *Count) Query(query bsoncore.Document) *Count {
	if c == nil {
		c = new(Count)
	}

	c.query = query
	return c
}

// Session sets the session for this operation.
func (c *Count) Session(session *session.Client) *Count {
	if c == nil {
		c = new(Count)
	}

	c.session = session
	return c
}

// ClusterClock sets the cluster clock for this operation.
func (c *Count) ClusterClock(clock *session.ClusterClock) *Count {
	if c == nil {
		c = new(Count)
	}

	c.clock = clock
	return c
}

// Collection sets the collection that this command will run against.
func (c *Count) Collection(collection string) *Count {
	if c == nil {
		c = new(Count)
	}

	c.collection = collection
	return c
}

// CommandMonitor sets the monitor to use for APM events.
func (c *Count) CommandMonitor(monitor *event.CommandMonitor) *Count {
	if c == nil {
		c = new(Count)
	}

	c.monitor = monitor
	return c
}

// Crypt sets the Crypt object to use for automatic encryption and decryption.
func (c *Count) Crypt(crypt driver.Crypt) *Count {
	if c == nil {
		c = new(Count)
	}

	c.crypt = crypt
	return c
}

// Database sets the database to run this operation against.
func (c *Count) Database(database string) *Count {
	if c == nil {
		c = new(Count)
	}

	c.database = database
	return c
}

// Deployment sets the deployment to use for this operation.
func (c *Count) Deployment(deployment driver.Deployment) *Count {
	if c == nil {
		c = new(Count)
	}

	c.deployment = deployment
	return c
}

// ReadConcern specifies the read concern for this operation.
func (c *Count) ReadConcern(readConcern *readconcern.ReadConcern) *Count {
	if c == nil {
		c = new(Count)
	}

	c.readConcern = readConcern
	return c
}

// ReadPreference set the read prefernce used with this operation.
func (c *Count) ReadPreference(readPreference *readpref.ReadPref) *Count {
	if c == nil {
		c = new(Count)
	}

	c.readPreference = readPreference
	return c
}

// ServerSelector sets the selector used to retrieve a server.
func (c *Count) ServerSelector(selector description.ServerSelector) *Count {
	if c == nil {
		c = new(Count)
	}

	c.selector = selector
	return c
}

// Retry enables retryable mode for this operation. Retries are handled automatically in driver.Operation.Execute based
// on how the operation is set.
func (c *Count) Retry(retry driver.RetryMode) *Count {
	if c == nil {
		c = new(Count)
	}

	c.retry = &retry
	return c
}

// ServerAPI sets the server API version for this operation.
func (c *Count) ServerAPI(serverAPI *driver.ServerAPIOptions) *Count {
	if c == nil {
		c = new(Count)
	}

	c.serverAPI = serverAPI
	return c
}
