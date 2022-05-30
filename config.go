package stunner

import (
	"os"
	"fmt"
	"regexp"
	"strconv"
	"encoding/json"
        
	// "github.com/pion/logging"
	// "github.com/pion/turn/v2"
	"sigs.k8s.io/yaml"

	"github.com/l7mp/stunner/internal/object"
	"github.com/l7mp/stunner/pkg/apis/v1alpha1"
)

// NewDefaultStunnerConfig builds a default configuration from a STUNner URI. Example: the URI
// `turn://user:pass@127.0.0.1:3478` will be parsed into a STUNner configuration with a server
// running on the localhost at port 3478, with plain-text authentication using the
// username/password pair `user:pass`.
func NewDefaultStunnerConfig(uri string) (*v1alpha1.StunnerConfig, error) {
        u, err := ParseUri(uri)
	if err != nil {
		return nil, fmt.Errorf("Invalid URI '%s': %s", uri, err)
	}

	if u.Protocol != "udp" {
		return nil, fmt.Errorf("Invalid protocol: %s", u.Protocol)
	}

	if u.Username == "" || u.Password == "" {
		return nil, fmt.Errorf("Username/password must be set: '%s'", uri)
	}

	c := &v1alpha1.StunnerConfig{
                ApiVersion: v1alpha1.ApiVersion,
                Admin: v1alpha1.AdminConfig{
                        LogLevel: v1alpha1.DefaultLogLevel,
                },
                Auth: v1alpha1.AuthConfig{
                        Type: "plaintext",
                        Realm: v1alpha1.DefaultRealm,
                        Credentials: map[string]string{
                                "username": u.Username,
                                "password": u.Password,
                        },
                },
                Listeners: []v1alpha1.ListenerConfig{{
                        Name: "default-listener",
                        Protocol: u.Protocol,
                        Addr: u.Address,
                        Port: u.Port,
                        Routes: []string{"allow-any"},
                }},
                Clusters: []v1alpha1.ClusterConfig{{
                        Name: "allow-any",
                        Type: "STATIC",
                        Endpoints: []string{"0.0.0.0/0"},
                }},
	}

        if err := c.Validate(); err != nil {
                return nil, err
        }

        return c, nil
}

// LoadConfig loads a configuration from a file, substituting environment variables for
// placeholders in the configuration file. Returns the new configuration or error if load fails
func LoadConfig(config string) (*v1alpha1.StunnerConfig, error) {
        c, err := os.ReadFile(config)
        if err != nil {
                return nil, fmt.Errorf("could not read config: %s\n", err.Error())
        }

        // substitute environtment variables
        // default port: STUNNER_PUBLIC_PORT -> STUNNER_PORT
        re := regexp.MustCompile(`^[0-9]+$`)
        port, ok := os.LookupEnv("STUNNER_PORT")
        if !ok || (ok && port == "") || (ok && !re.Match([]byte(port))) {
                publicPort := v1alpha1.DefaultPort
                publicPortStr, ok := os.LookupEnv("STUNNER_PUBLIC_PORT")
                if ok {
                        if p, err := strconv.Atoi(publicPortStr); err == nil {
                                publicPort = p
                        }
                }
                os.Setenv("STUNNER_PORT", fmt.Sprintf("%d", publicPort))
        }

        e := os.ExpandEnv(string(c))

        s := v1alpha1.StunnerConfig{}
        // try YAML first
        if err = yaml.Unmarshal([]byte(e), &s); err != nil {
                // if it fails, try to json
                if errJ := json.Unmarshal([]byte(e), &s); err != nil {
                        return nil, fmt.Errorf("could not parse config file at '%s': "+
                                "YAML parse error: %s, JSON parse error: %s\n",
                                config, err.Error(), errJ.Error())
                }
        }

        return &s, nil
}

// GetConfig returns the configuration of the running STUNner daemon
func (s *Stunner) GetConfig() *v1alpha1.StunnerConfig {
	s.log.Tracef("GetConfig")

        listeners := s.listenerManager.Keys()
        clusters  := s.clusterManager.Keys()

	c := v1alpha1.StunnerConfig{
		ApiVersion: s.version,
		Admin:      *s.GetAdmin().GetConfig().(*v1alpha1.AdminConfig),
                Auth:       *s.GetAuth().GetConfig().(*v1alpha1.AuthConfig),
                Listeners:  make([]v1alpha1.ListenerConfig, len(listeners)),
                Clusters:   make([]v1alpha1.ClusterConfig, len(clusters)),
	}

	for i, name := range listeners {
		c.Listeners[i] = *s.GetListener(name).GetConfig().(*v1alpha1.ListenerConfig)
        }

	for i, name := range clusters {
		c.Clusters[i] = *s.GetCluster(name).GetConfig().(*v1alpha1.ClusterConfig)
	}

        return &c
}

// Reconcile handles the updates to the STUNner configuration. Some updates are destructive so the server must be closed and restarted with the new configuration manually (see the documentation of the corresponding STUNner objects for when STUNner may restart after a reconciliation). Reconcile returns nil if no action is required by the caller, v1alpha1.ErrRestartRequired to indicate that the caller must issue a Close/Start cycle to install the reconciled configuration, and a general error if the reconciliation has failed
func (s *Stunner) Reconcile(req *v1alpha1.StunnerConfig) error {
	s.log.Debugf("reconciling STUNner for config: %#v ", req)

        // validate config
        if err := req.Validate(); err != nil {
                return err
        }

        restart := false

        // admin
        newAdmin, err := s.adminManager.Reconcile([]v1alpha1.Config{&req.Admin})
        if err != nil {
                if err == v1alpha1.ErrRestartRequired {
                        restart = true
                } else {
                        return fmt.Errorf("could not reconcile admin config: %s", err.Error())
                }
        }

        for _, c := range newAdmin {
                o, err := object.NewAdmin(c, s.logger)
                if err != nil && err != v1alpha1.ErrRestartRequired {
                        return err
                }
                s.adminManager.Upsert(o)
        }
        s.logger = NewLoggerFactory(s.GetAdmin().LogLevel)
        s.log    = s.logger.NewLogger("stunner")

        // auth
        newAuth, err := s.authManager.Reconcile([]v1alpha1.Config{&req.Auth})
        if err != nil {
                if err == v1alpha1.ErrRestartRequired {
                        restart = true
                } else {
                        return fmt.Errorf("could not reconcile auth config: %s", err.Error())
                }
        }

        for _, c := range newAuth {
                o, err := object.NewAuth(c, s.logger)
                if err != nil && err != v1alpha1.ErrRestartRequired {
                        return err
                }
                s.authManager.Upsert(o)
        }

        // listener
        lconf := make([]v1alpha1.Config, len(req.Listeners))
        for i, _ := range req.Listeners {
                lconf[i] = &(req.Listeners[i])
        }        
        newListener, err := s.listenerManager.Reconcile(lconf)
        if err != nil {
                if err == v1alpha1.ErrRestartRequired {
                        restart = true
                } else {
                        return fmt.Errorf("could not reconcile listener config: %s", err.Error())
                }
        }

        for _, c := range newListener {
                o, err := object.NewListener(c, s.net, s.logger)
                if err != nil && err != v1alpha1.ErrRestartRequired {
                        return err
                }
                s.listenerManager.Upsert(o)
                // new listeners require a restart
                restart = true
        }

        if len(s.listenerManager.Keys()) == 0 {
                s.log.Warn("running with no listeners")
        }

        // cluster
        cconf := make([]v1alpha1.Config, len(req.Clusters))
        for i, _ := range req.Clusters {
                cconf[i] = &(req.Clusters[i])
        }
        newCluster, err := s.clusterManager.Reconcile(cconf)
        if err != nil {
                if err == v1alpha1.ErrRestartRequired {
                        restart = true
                } else {
                        return fmt.Errorf("could not reconcile cluster config: %s", err.Error())
                }
        }

        for _, c := range newCluster {
                o, err := object.NewCluster(c, s.logger)
                if err != nil && err != v1alpha1.ErrRestartRequired {
                        return err
                }
                s.clusterManager.Upsert(o)
        }

        if len(s.clusterManager.Keys()) == 0 {
                s.log.Warn("running with no clusters: received traffic will be dropped")
        }

        if restart {
                return v1alpha1.ErrRestartRequired
        }

        return nil
}

