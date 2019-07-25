// Copyright 2018 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package internal

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	ctx "golang.org/x/net/context"

	ext_authz "github.com/envoyproxy/go-control-plane/envoy/service/auth/v2"
	google_rpc "github.com/gogo/googleapis/google/rpc"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/metrics"
	"github.com/open-policy-agent/opa/plugins"
	"github.com/open-policy-agent/opa/plugins/logs"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/server"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/util"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

const defaultAddr = ":9191"
const defaultQuery = "data.istio.authz.allow"
const defaultDryRun = false

var revisionPath = storage.MustParsePath("/system/bundle/manifest/revision")

type evalResult struct {
	revision   string
	decisionID string
	txnID      uint64
	decision   string
	metrics    metrics.Metrics
}

// Validate receives a slice of bytes representing the plugin's
// configuration and returns a configuration value that can be used to
// instantiate the plugin.
func Validate(m *plugins.Manager, bs []byte) (*Config, error) {

	cfg := Config{
		Addr:   defaultAddr,
		Query:  defaultQuery,
		DryRun: defaultDryRun,
	}

	if err := util.Unmarshal(bs, &cfg); err != nil {
		return nil, err
	}

	parsedQuery, err := ast.ParseBody(cfg.Query)
	if err != nil {
		return nil, err
	}
	cfg.parsedQuery = parsedQuery

	return &cfg, nil
}

// New returns a Plugin that implements the Envoy ext_authz API.
func New(m *plugins.Manager, cfg *Config) plugins.Plugin {

	plugin := &envoyExtAuthzGrpcServer{
		manager: m,
		cfg:     *cfg,
		server:  grpc.NewServer(),
	}

	ext_authz.RegisterAuthorizationServer(plugin.server, plugin)

	return plugin
}

// Config represents the plugin configuration.
type Config struct {
	Addr        string `json:"addr"`
	Query       string `json:"query"`
	DryRun      bool   `json:"dry-run"`
	parsedQuery ast.Body
}

type envoyExtAuthzGrpcServer struct {
	cfg     Config
	server  *grpc.Server
	manager *plugins.Manager
}

func (p *envoyExtAuthzGrpcServer) Start(ctx context.Context) error {
	go p.listen()
	return nil
}

func (p *envoyExtAuthzGrpcServer) Stop(ctx context.Context) {
	p.server.Stop()
}

func (p *envoyExtAuthzGrpcServer) Reconfigure(ctx context.Context, config interface{}) {
	return
}

func (p *envoyExtAuthzGrpcServer) listen() {

	// The listener is closed automatically by Serve when it returns.
	l, err := net.Listen("tcp", p.cfg.Addr)
	if err != nil {
		logrus.WithField("err", err).Fatalf("Unable to create listener.")
	}

	logrus.WithFields(logrus.Fields{
		"addr":    p.cfg.Addr,
		"query":   p.cfg.Query,
		"dry-run": p.cfg.DryRun,
	}).Infof("Starting gRPC server.")

	if err := p.server.Serve(l); err != nil {
		logrus.WithField("err", err).Fatalf("Listener failed.")
	}

	logrus.Info("Listener exited.")
}

func (p *envoyExtAuthzGrpcServer) Check(ctx ctx.Context, req *ext_authz.CheckRequest) (*ext_authz.CheckResponse, error) {
	start := time.Now()

	bs, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var input map[string]interface{}

	err = util.UnmarshalJSON(bs, &input)
	if err != nil {
		return nil, err
	}

	input["parsed_path"] = getParsedPath(req)

	inputValue, err := ast.InterfaceToValue(input)
	if err != nil {
		return nil, err
	}

	result, err := p.eval(ctx, inputValue)
	if err != nil {
		return nil, err
	}

	status := int32(google_rpc.PERMISSION_DENIED)

	var allow bool
	allow, _ = strconv.ParseBool(result.decision)
	if p.cfg.DryRun || allow {
		status = int32(google_rpc.OK)
	}

	resp := &ext_authz.CheckResponse{
		Status: &google_rpc.Status{Code: status},
	}

	err = p.log(ctx, input, result, err)
	if err != nil {
		resp := &ext_authz.CheckResponse{
			Status: &google_rpc.Status{
				Code:    int32(google_rpc.UNKNOWN),
				Message: err.Error(),
			},
		}
		return resp, nil
	}

	logrus.WithFields(logrus.Fields{
		"query":               p.cfg.Query,
		"dry-run":             p.cfg.DryRun,
		"decision":            result.decision,
		"err":                 err,
		"txn":                 result.txnID,
		"metrics":             result.metrics.All(),
		"total_decision_time": time.Since(start),
	}).Debugf("Returning policy decision.")

	return resp, nil
}

func (p *envoyExtAuthzGrpcServer) eval(ctx context.Context, input ast.Value, opts ...func(*rego.Rego)) (*evalResult, error) {
	result := &evalResult{}
	result.metrics = metrics.New()

	err := storage.Txn(ctx, p.manager.Store, storage.TransactionParams{}, func(txn storage.Transaction) error {

		var err error
		var decision interface{}
		var ok bool

		result.revision, err = getRevision(ctx, p.manager.Store, txn)
		if err != nil {
			return err
		}

		result.decisionID, err = uuid4()
		if err != nil {
			return err
		}

		result.txnID = txn.ID()

		logrus.WithFields(logrus.Fields{
			"input":   input,
			"query":   p.cfg.Query,
			"dry-run": p.cfg.DryRun,
			"txn":     result.txnID,
		}).Debugf("Executing policy query.")

		opts = append(opts,
			rego.Metrics(result.metrics),
			rego.ParsedQuery(p.cfg.parsedQuery),
			rego.ParsedInput(input),
			rego.Compiler(p.manager.GetCompiler()),
			rego.Store(p.manager.Store),
			rego.Transaction(txn),
			rego.Runtime(p.manager.Info))

		rs, err := rego.New(opts...).Eval(ctx)

		// In "dry-run" mode, we just log all failure conditions
		// even ones that would typically be considered an error
		if err != nil {
			err = fmt.Errorf("Error during Rego eval: %s", err.Error())
		} else if len(rs) == 0 {
			err = fmt.Errorf("Rego eval length was zero")
		} else if decision, ok = rs[0].Expressions[0].Value.(interface{}); !ok || len(rs) > 1 {
			err = fmt.Errorf("Decision was not interface{} OR len(rs) > 1")
		}

		if err != nil {
			if p.cfg.DryRun {
				logrus.Warnf(err.Error())
			} else {
				return err
			}
		}

		jsonBytes, err := json.Marshal(decision)
		if err == nil {
			result.decision = string(jsonBytes)
		} else {
			// Fallback to an empty string and log a warning
			result.decision = ""
			logrus.WithField("decision", decision).Warnf("Decision could not be encoded as json: %s", err.Error())
		}

		return nil
	})

	return result, err
}

func (p *envoyExtAuthzGrpcServer) log(ctx context.Context, input interface{}, result *evalResult, err error) error {
	plugin := logs.Lookup(p.manager)
	if plugin == nil {
		return nil
	}

	info := &server.Info{
		Revision:   result.revision,
		DecisionID: result.decisionID,
		Timestamp:  time.Now(),
		Query:      p.cfg.Query,
		Input:      &input,
		Error:      err,
		Metrics:    result.metrics,
	}

	if err == nil {
		var x interface{} = result.decision
		info.Results = &x
	}

	return plugin.Log(ctx, info)
}

func uuid4() (string, error) {
	bs := make([]byte, 16)
	n, err := io.ReadFull(rand.Reader, bs)
	if n != len(bs) || err != nil {
		return "", err
	}
	bs[8] = bs[8]&^0xc0 | 0x80
	bs[6] = bs[6]&^0xf0 | 0x40
	return fmt.Sprintf("%x-%x-%x-%x-%x", bs[0:4], bs[4:6], bs[6:8], bs[8:10], bs[10:]), nil
}

func getRevision(ctx context.Context, store storage.Store, txn storage.Transaction) (string, error) {
	value, err := store.Read(ctx, txn, revisionPath)
	if err != nil {
		if storage.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	revision, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("bad revision")
	}
	return revision, nil
}

func getParsedPath(req *ext_authz.CheckRequest) []interface{} {
	path := req.GetAttributes().GetRequest().GetHttp().GetPath()
	parsedPath := strings.Split(strings.TrimLeft(path, "/"), "/")
	parsedPathInterface := make([]interface{}, len(parsedPath))
	for i, v := range parsedPath {
		parsedPathInterface[i] = v
	}
	return parsedPathInterface
}
