// Copyright (c) 2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"

	"decred.org/dcrwallet/rpc/walletrpc"
	"github.com/decred/dcrd/rpcclient/v6"
	"github.com/decred/dcrpool/gui"
	"github.com/decred/dcrpool/pool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// miningPool represents a decred Proof-of-Work mining pool.
type miningPool struct {
	cfg    *config
	ctx    context.Context
	cancel context.CancelFunc
	hub    *pool.Hub
	gui    *gui.GUI
}

// newPool initializes the mining pool.
func newPool(db pool.Database, cfg *config) (*miningPool, error) {
	p := new(miningPool)
	p.cfg = cfg
	dcrdRPCCfg := &rpcclient.ConnConfig{
		Host:         cfg.DcrdRPCHost,
		Endpoint:     "ws",
		User:         cfg.RPCUser,
		Pass:         cfg.RPCPass,
		Certificates: cfg.dcrdRPCCerts,
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	powLimit := cfg.net.PowLimit
	powLimitF, _ := new(big.Float).SetInt(powLimit).Float64()
	iterations := math.Pow(2, 256-math.Floor(math.Log2(powLimitF)))
	addPort := func(ports map[string]uint32, key string, entry uint32) error {
		var match bool
		var miner string
		for m, port := range ports {
			if port == entry {
				match = true
				miner = m
				break
			}
		}
		if match {
			return fmt.Errorf("%s and %s share port %d", key, miner, entry)
		}
		ports[key] = entry
		return nil
	}

	// Ensure provided miner ports are unique.
	minerPorts := make(map[string]uint32)
	_ = addPort(minerPorts, pool.CPU, cfg.CPUPort)
	err := addPort(minerPorts, pool.InnosiliconD9, cfg.D9Port)
	if err != nil {
		return nil, err
	}
	err = addPort(minerPorts, pool.AntminerDR3, cfg.DR3Port)
	if err != nil {
		return nil, err
	}
	err = addPort(minerPorts, pool.AntminerDR5, cfg.DR5Port)
	if err != nil {
		return nil, err
	}
	err = addPort(minerPorts, pool.WhatsminerD1, cfg.D1Port)
	if err != nil {
		return nil, err
	}
	err = addPort(minerPorts, pool.ObeliskDCR1, cfg.DCR1Port)
	if err != nil {
		return nil, err
	}

	hcfg := &pool.HubConfig{
		DB:                    db,
		ActiveNet:             cfg.net.Params,
		PoolFee:               cfg.PoolFee,
		MaxGenTime:            cfg.MaxGenTime,
		PaymentMethod:         cfg.PaymentMethod,
		LastNPeriod:           cfg.LastNPeriod,
		WalletPass:            cfg.WalletPass,
		PoolFeeAddrs:          cfg.poolFeeAddrs,
		SoloPool:              cfg.SoloPool,
		NonceIterations:       iterations,
		MinerPorts:            minerPorts,
		MaxConnectionsPerHost: cfg.MaxConnectionsPerHost,
		WalletAccount:         cfg.WalletAccount,
		CoinbaseConfTimeout:   cfg.CoinbaseConfTimeout,
	}
	p.hub, err = pool.NewHub(p.cancel, hcfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize hub: %v", err)
	}

	// Establish a connection to the mining node.
	ntfnHandlers := p.hub.CreateNotificationHandlers()
	nodeConn, err := rpcclient.New(dcrdRPCCfg, ntfnHandlers)
	if err != nil {
		return nil, err
	}

	if err := nodeConn.NotifyWork(p.ctx); err != nil {
		nodeConn.Shutdown()
		return nil, err
	}
	if err := nodeConn.NotifyBlocks(p.ctx); err != nil {
		nodeConn.Shutdown()
		return nil, err
	}

	p.hub.SetNodeConnection(nodeConn)

	// Establish a connection to the wallet if the pool is mining as a
	// publicly available mining pool.
	if !cfg.SoloPool {
		serverCAs := x509.NewCertPool()
		serverCert, err := ioutil.ReadFile(cfg.WalletRPCCert)
		if err != nil {
			return nil, err
		}
		if !serverCAs.AppendCertsFromPEM(serverCert) {
			return nil, fmt.Errorf("no certificates found in %s",
				cfg.WalletRPCCert)
		}
		keypair, err := tls.LoadX509KeyPair(cfg.WalletTLSCert, cfg.WalletTLSKey)
		if err != nil {
			return nil, fmt.Errorf("unable to read keypair: %v", err)
		}
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{keypair},
			RootCAs:      serverCAs,
		})
		grpc, err := grpc.Dial(cfg.WalletGRPCHost,
			grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, err
		}

		// Perform a Balance request to check connectivity and account
		// existence.
		walletConn := walletrpc.NewWalletServiceClient(grpc)
		req := &walletrpc.BalanceRequest{
			AccountNumber:         cfg.WalletAccount,
			RequiredConfirmations: 1,
		}
		_, err = walletConn.Balance(p.ctx, req)
		if err != nil {
			return nil, err
		}

		p.hub.SetWalletConnection(walletConn, grpc.Close)

		confNotifs, err := walletConn.ConfirmationNotifications(p.ctx)
		if err != nil {
			return nil, err
		}

		p.hub.SetTxConfNotifClient(confNotifs)
	}

	err = p.hub.FetchWork(p.ctx)
	if err != nil {
		return nil, err
	}
	err = p.hub.Listen()
	if err != nil {
		return nil, err
	}

	csrfSecret, err := p.hub.CSRFSecret()
	if err != nil {
		return nil, err
	}

	gcfg := &gui.Config{
		SoloPool:              cfg.SoloPool,
		GUIDir:                cfg.GUIDir,
		AdminPass:             cfg.AdminPass,
		GUIPort:               cfg.GUIPort,
		UseLEHTTPS:            cfg.UseLEHTTPS,
		Domain:                cfg.Domain,
		TLSCertFile:           cfg.TLSCert,
		TLSKeyFile:            cfg.TLSKey,
		ActiveNet:             cfg.net.Params,
		PaymentMethod:         cfg.PaymentMethod,
		Designation:           cfg.Designation,
		PoolFee:               cfg.PoolFee,
		CSRFSecret:            csrfSecret,
		MinerPorts:            minerPorts,
		WithinLimit:           p.hub.WithinLimit,
		FetchLastWorkHeight:   p.hub.FetchLastWorkHeight,
		FetchLastPaymentInfo:  p.hub.FetchLastPaymentInfo,
		FetchMinedWork:        p.hub.FetchMinedWork,
		FetchWorkQuotas:       p.hub.FetchWorkQuotas,
		FetchClients:          p.hub.FetchClients,
		AccountExists:         p.hub.AccountExists,
		FetchArchivedPayments: p.hub.FetchArchivedPayments,
		FetchPendingPayments:  p.hub.FetchPendingPayments,
		FetchCacheChannel:     p.hub.FetchCacheChannel,
	}

	if !cfg.UsePostgres {
		gcfg.HTTPBackupDB = p.hub.HTTPBackupDB
	}

	p.gui, err = gui.NewGUI(gcfg)
	if err != nil {
		p.hub.CloseListeners()
		return nil, err
	}
	return p, nil
}

func main() {
	// Listen for interrupt signals.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Load configuration and parse command line. This also initializes logging
	// and configures it accordingly.
	cfg, _, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

	var db pool.Database
	if cfg.UsePostgres {
		db, err = pool.InitPostgresDB(cfg.PGHost, cfg.PGPort, cfg.PGUser,
			cfg.PGPass, cfg.PGDBName)
	} else {
		db, err = pool.InitBoltDB(cfg.DBFile)
	}

	if err != nil {
		mpLog.Errorf("failed to initialize database: %v", err)
		os.Exit(1)
	}

	p, err := newPool(db, cfg)
	if err != nil {
		mpLog.Errorf("failed to initialize pool: %v", err)
		os.Exit(1)
	}

	if cfg.Profile != "" {
		// Start the profiler.
		go func() {
			listenAddr := cfg.Profile
			mpLog.Infof("Creating profiling server listening "+
				"on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			err := http.ListenAndServe(listenAddr, nil)
			if err != nil {
				mpLog.Criticalf(err.Error())
				p.cancel()
			}
		}()
	}

	mpLog.Infof("Version: %s", version())
	mpLog.Infof("Runtime: Go version %s", runtime.Version())
	mpLog.Infof("Home dir: %s", cfg.HomeDir)
	mpLog.Infof("Started dcrpool.")

	go func() {
		select {
		case <-p.ctx.Done():
			return

		case <-interrupt:
			p.cancel()
		}
	}()
	p.gui.Run(p.ctx)
	p.hub.Run(p.ctx)

	// hub.Run() blocks until the pool is fully shut down. When it returns,
	// write a backup of the DB (if not using postgres), and then close the DB.
	if !cfg.UsePostgres {
		mpLog.Tracef("Backing up database.")
		err = db.Backup(pool.BoltBackupFile)
		if err != nil {
			mpLog.Errorf("failed to write database backup file: %v", err)
		}
	}

	db.Close()
}
