// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package session caches vSphere objects to avoid having to repeatedly
// make govmomi client calls.
//
// To obtain a Session, call Create with a Config. The config
// contains the SDK URL (Service) and the desired vSphere resources.
// Create then connects to Service and stores govmomi objects for
// each corresponding value in Config. The Session is returned and
// the user can use the cached govmomi objects in the exported fields of
// Session instead of directly using a govmomi Client.
//
package session

import (
	"crypto/tls"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/vic/pkg/errors"
)

// Config contains the configuration used to create a Session.
type Config struct {
	// SDK URL or proxy
	Service string
	// Allow insecure connection to Service
	Insecure bool
	// Keep alive duration
	Keepalive time.Duration

	ClusterPath    string
	DatacenterPath string
	DatastorePath  string
	HostPath       string
	NetworkPath    string
	PoolPath       string

	CertFile string
	KeyFile  string
}

// HasCertificate checks for presence of a certificate and keyfile
func (c *Config) HasCertificate() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// Session caches vSphere objects obtained by querying the SDK.
type Session struct {
	*govmomi.Client

	*Config

	Cluster    *object.ComputeResource
	Datacenter *object.Datacenter
	Datastore  *object.Datastore
	Host       *object.HostSystem
	Network    object.NetworkReference
	Pool       *object.ResourcePool

	Finder *find.Finder
}

// NewSession creates a new Session struct. If config is nil,
// it creates a Flags object from the command line arguments or
// environment, and uses that instead to create a Session.
func NewSession(config *Config) *Session {
	return &Session{Config: config}
}

// Vim25 returns the vim25.Client to the caller
func (s *Session) Vim25() *vim25.Client {
	return s.Client.Client
}

// IsVC returns whether the session is backed by VC
func (s *Session) IsVC() bool {
	return s.Client.IsVC()
}

// IsVSAN returns whether the datastore used in the session is backed by VSAN
func (s *Session) IsVSAN(ctx context.Context) bool {
	dsType, _ := s.Datastore.Type(ctx)

	return dsType == types.HostFileSystemVolumeFileSystemTypeVsan
}

// Create accepts a Config and returns a Session with the cached vSphere resources.
func (s *Session) Create(ctx context.Context) (*Session, error) {
	_, err := s.Connect(ctx)
	if err != nil {
		return nil, err
	}

	// we're treating this as an atomic behaviour, so log out if we failed
	defer func() {
		if err != nil {
			s.Client.Logout(ctx)
		}
	}()

	_, err = s.Populate(ctx)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// Connect establishes the connection for the session but nothing more
func (s *Session) Connect(ctx context.Context) (*Session, error) {
	soapURL, err := soap.ParseURL(s.Service)
	if soapURL == nil || err != nil {
		return nil, errors.Errorf("SDK URL (%s) could not be parsed: %s", s.Service, err)
	}

	// we can't set a keep alive if we log in directly with client creation
	user := soapURL.User
	soapURL.User = nil

	// 1st connect without any userinfo to get the API type
	s.Client, err = govmomi.NewClient(ctx, soapURL, s.Insecure)
	if err != nil {
		return nil, errors.Errorf("Failed to connect to %s: %s", soapURL.String(), err)
	}

	if s.HasCertificate() {
		if !s.Client.IsVC() {
			return nil, errors.Errorf("Certificate based authentication not yet supported with ESXi")
		}

		// load the certificates
		cert, err2 := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
		if err2 != nil {
			return nil, errors.Errorf("Unable to load X509 key pair(%s,%s): %s", s.CertFile, s.KeyFile, err2)
		}

		// create the new client
		s.Client, err = govmomi.NewClientWithCertificate(ctx, soapURL, s.Insecure, cert)
		if err != nil {
			return nil, errors.Errorf("Failed to connect to %s: %s", soapURL.String(), err)
		}
	}

	if s.Keepalive != 0 {
		// now that we've verified everything, enable keepalive
		s.RoundTripper = session.KeepAlive(s.Client.RoundTripper, s.Keepalive)
	}

	// and now that the keepalive is registered we can log in to trigger it
	if !s.HasCertificate() {
		err = s.Client.Login(ctx, user)
	} else {
		err = s.LoginExtensionByCertificate(ctx, user.Username(), "")
	}
	if err != nil {
		return nil, errors.Errorf("Failed to log in to %s: %s", soapURL.String(), err)
	}

	s.Finder = find.NewFinder(s.Vim25(), true)

	return s, nil
}

// Populate resolves the set of cached resources that should be presented
// This returns accumulated error detail if there is ambiguity, but sets all
// unambiguous or correct resources.
func (s *Session) Populate(ctx context.Context) (*Session, error) {
	// Populate s
	var errs []string
	var err error

	finder := s.Finder

	s.Datacenter, err = finder.DatacenterOrDefault(ctx, s.DatacenterPath)
	if err != nil {
		errs = append(errs, err.Error())
	} else {
		finder.SetDatacenter(s.Datacenter)
	}

	s.Cluster, err = finder.ComputeResourceOrDefault(ctx, s.ClusterPath)
	if err != nil {
		errs = append(errs, err.Error())
	}

	s.Datastore, err = finder.DatastoreOrDefault(ctx, s.DatastorePath)
	if err != nil {
		errs = append(errs, err.Error())
	}

	s.Host, err = finder.HostSystemOrDefault(ctx, s.HostPath)
	if err != nil {
		if _, ok := err.(*find.DefaultMultipleFoundError); !ok || !s.IsVC() {
			errs = append(errs, err.Error())
		}
	}

	if s.NetworkPath != "" {
		s.Network, err = finder.NetworkOrDefault(ctx, s.NetworkPath)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	s.Pool, err = finder.ResourcePoolOrDefault(ctx, s.PoolPath)
	if err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}

	return s, nil
}
