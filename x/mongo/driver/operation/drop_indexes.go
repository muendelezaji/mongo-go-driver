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
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/mongo/driver/session"
)

// DropIndexes performs an dropIndexes operation.
type DropIndexes struct {
	index        *string
	maxTimeMS    *int64
	session      *session.Client
	clock        *session.ClusterClock
	collection   string
	monitor      *event.CommandMonitor
	crypt        driver.Crypt
	database     string
	deployment   driver.Deployment
	selector     description.ServerSelector
	writeConcern *writeconcern.WriteConcern
	result       DropIndexesResult
	serverAPI    *driver.ServerAPIOptions
}

// DropIndexesResult represents a dropIndexes result returned by the server.
type DropIndexesResult struct {
	// Number of indexes that existed before the drop was executed.
	NIndexesWas int32
}

func buildDropIndexesResult(response bsoncore.Document) (DropIndexesResult, error) {
	elements, err := response.Elements()
	if err != nil {
		return DropIndexesResult{}, err
	}
	dir := DropIndexesResult{}
	for _, element := range elements {
		switch element.Key() {
		case "nIndexesWas":
			var ok bool
			dir.NIndexesWas, ok = element.Value().AsInt32OK()
			if !ok {
				return dir, fmt.Errorf("response field 'nIndexesWas' is type int32, but received BSON type %s", element.Value().Type)
			}
		}
	}
	return dir, nil
}

// NewDropIndexes constructs and returns a new DropIndexes.
func NewDropIndexes(index string) *DropIndexes {
	return &DropIndexes{
		index: &index,
	}
}

// Result returns the result of executing this operation.
func (di *DropIndexes) Result() DropIndexesResult { return di.result }

func (di *DropIndexes) processResponse(info driver.ResponseInfo) error {
	var err error
	di.result, err = buildDropIndexesResult(info.ServerResponse)
	return err
}

// Execute runs this operations and returns an error if the operaiton did not execute successfully.
func (di *DropIndexes) Execute(ctx context.Context) error {
	if di.deployment == nil {
		return errors.New("the DropIndexes operation must have a Deployment set before Execute can be called")
	}

	return driver.Operation{
		CommandFn:         di.command,
		ProcessResponseFn: di.processResponse,
		Client:            di.session,
		Clock:             di.clock,
		CommandMonitor:    di.monitor,
		Crypt:             di.crypt,
		Database:          di.database,
		Deployment:        di.deployment,
		Selector:          di.selector,
		WriteConcern:      di.writeConcern,
		ServerAPI:         di.serverAPI,
	}.Execute(ctx, nil)

}

func (di *DropIndexes) command(dst []byte, desc description.SelectedServer) ([]byte, error) {
	dst = bsoncore.AppendStringElement(dst, "dropIndexes", di.collection)
	if di.index != nil {
		dst = bsoncore.AppendStringElement(dst, "index", *di.index)
	}
	if di.maxTimeMS != nil {
		dst = bsoncore.AppendInt64Element(dst, "maxTimeMS", *di.maxTimeMS)
	}
	return dst, nil
}

// Index specifies the name of the index to drop. If '*' is specified, all indexes will be dropped.
//
func (di *DropIndexes) Index(index string) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.index = &index
	return di
}

// MaxTimeMS specifies the maximum amount of time to allow the query to run.
func (di *DropIndexes) MaxTimeMS(maxTimeMS int64) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.maxTimeMS = &maxTimeMS
	return di
}

// Session sets the session for this operation.
func (di *DropIndexes) Session(session *session.Client) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.session = session
	return di
}

// ClusterClock sets the cluster clock for this operation.
func (di *DropIndexes) ClusterClock(clock *session.ClusterClock) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.clock = clock
	return di
}

// Collection sets the collection that this command will run against.
func (di *DropIndexes) Collection(collection string) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.collection = collection
	return di
}

// CommandMonitor sets the monitor to use for APM events.
func (di *DropIndexes) CommandMonitor(monitor *event.CommandMonitor) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.monitor = monitor
	return di
}

// Crypt sets the Crypt object to use for automatic encryption and decryption.
func (di *DropIndexes) Crypt(crypt driver.Crypt) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.crypt = crypt
	return di
}

// Database sets the database to run this operation against.
func (di *DropIndexes) Database(database string) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.database = database
	return di
}

// Deployment sets the deployment to use for this operation.
func (di *DropIndexes) Deployment(deployment driver.Deployment) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.deployment = deployment
	return di
}

// ServerSelector sets the selector used to retrieve a server.
func (di *DropIndexes) ServerSelector(selector description.ServerSelector) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.selector = selector
	return di
}

// WriteConcern sets the write concern for this operation.
func (di *DropIndexes) WriteConcern(writeConcern *writeconcern.WriteConcern) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.writeConcern = writeConcern
	return di
}

// ServerAPI sets the server API version for this operation.
func (di *DropIndexes) ServerAPI(serverAPI *driver.ServerAPIOptions) *DropIndexes {
	if di == nil {
		di = new(DropIndexes)
	}

	di.serverAPI = serverAPI
	return di
}
