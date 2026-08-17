package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/beravelli/health-checker-router/internal/auth"
	"github.com/beravelli/health-checker-router/internal/models"
	"github.com/beravelli/health-checker-router/internal/routing"
	"github.com/beravelli/health-checker-router/internal/store"
)

// ------------------------------------------------------------- probes

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports readiness. Database reachability is the gate: without it
// the service can serve nothing useful, so it should leave the LB rotation.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready", "sseSubscribers": s.hub.Subscribers(),
	})
}

// ------------------------------------------------------------ session

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := s.dir.Authenticate(r.Context(), req.Username, req.Password)
	if err != nil {
		// Deliberately vague: distinguishing "no such user" from "wrong
		// password" is a user-enumeration gift.
		s.log.Warn("login failed", "username", req.Username, "error", err)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, expires, err := s.sessions.Create(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	auth.SetCookie(w, r, token, expires)

	writeJSON(w, http.StatusOK, map[string]any{
		"user":       user,
		"isAdmin":    user.InGroup(s.cfg.LDAPAdminGroup),
		"adminGroup": s.cfg.LDAPAdminGroup,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.TokenFrom(r); token != "" {
		if sess, ok := s.sessions.Get(token); ok {
			// Drop cached group memberships too, so a revoked admin does not
			// keep access until the LDAP cache expires.
			s.dir.Invalidate(sess.User.Username)
		}
		s.sessions.Delete(token)
	}
	auth.ClearCookie(w, r)
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"adminGroup":    s.cfg.LDAPAdminGroup,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
		"isAdmin":       user.InGroup(s.cfg.LDAPAdminGroup),
		"adminGroup":    s.cfg.LDAPAdminGroup,
	})
}

// --------------------------------------------------------------- read

func (s *Server) handleRegions(w http.ResponseWriter, r *http.Request) {
	regions, err := s.store.ListRegions(r.Context())
	if err != nil {
		s.fail(w, "list regions", err)
		return
	}
	writeJSON(w, http.StatusOK, regions)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.store.ListServices(r.Context())
	if err != nil {
		s.fail(w, "list services", err)
		return
	}
	writeJSON(w, http.StatusOK, services)
}

// handleHealth returns the composite snapshot backing Overview and Services.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap, err := s.agg.Compute(r.Context())
	if err != nil {
		s.fail(w, "compute health", err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	severity := r.URL.Query().Get("severity")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	list, err := s.incidents.List(r.Context(), severity, limit)
	if err != nil {
		s.fail(w, "list incidents", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request) {
	list, err := s.rollouts.List(r.Context())
	if err != nil {
		s.fail(w, "list rollouts", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetWeights(w http.ResponseWriter, r *http.Request) {
	weights, automatic, err := s.lb.Current(r.Context())
	if err != nil {
		s.fail(w, "get weights", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"weights": weights, "automatic": automatic,
	})
}

// handleFailOpen returns the policy and the current fail-open window, which is
// what drives the dashboard's banner.
func (s *Server) handleFailOpen(w http.ResponseWriter, r *http.Request) {
	active, err := s.store.ActiveFailOpen(r.Context())
	if err != nil {
		s.fail(w, "read fail-open state", err)
		return
	}
	policy := s.failopen.Policy()
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active != nil,
		"event":  active,
		"policy": map[string]any{
			"probeIntervalSeconds": int(s.cfg.ProbeInterval.Seconds()),
			"failuresToTrip":       s.cfg.FailuresToTrip,
			"successToRecover":     s.cfg.SuccessToRecover,
			"quorumRequired":       policy.QuorumRequired,
			"quorumTotal":          policy.QuorumTotal,
			"holdSeconds":          int(policy.Hold.Seconds()),
		},
	})
}

// handleRoutingEndpoints lists provider configs. Auth references are returned
// (they are names, not secrets) but never resolved values.
func (s *Server) handleRoutingEndpoints(w http.ResponseWriter, r *http.Request) {
	endpoints, err := s.store.ListRoutingEndpoints(r.Context())
	if err != nil {
		s.fail(w, "list routing endpoints", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoints":  endpoints,
		"registered": s.registry.Registered(),
	})
}

// ---------------------------------------------------------- lb control

type setWeightsRequest struct {
	Weights map[string]int `json:"weights"`
}

func (s *Server) handleSetWeights(w http.ResponseWriter, r *http.Request) {
	var req setWeightsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, _ := auth.UserFrom(r.Context())
	weights := routing.Weights(req.Weights)
	if err := weights.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.lb.SetManual(r.Context(), weights, user.Username); err != nil {
		s.fail(w, "set weights", err)
		return
	}
	s.audit(r, "lb.set_weights", "", map[string]any{"weights": req.Weights})
	writeJSON(w, http.StatusOK, map[string]any{"weights": weights, "automatic": false})
}

func (s *Server) handleResetWeights(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	if err := s.lb.ResetToAutomatic(r.Context(), user.Username); err != nil {
		s.fail(w, "reset weights", err)
		return
	}
	s.audit(r, "lb.reset_to_automatic", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"automatic": true})
}

type failoverRequest struct {
	Region string `json:"region"`
}

func (s *Server) handleFailover(w http.ResponseWriter, r *http.Request) {
	var req failoverRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Region == "" {
		writeError(w, http.StatusBadRequest, "region is required")
		return
	}

	user, _ := auth.UserFrom(r.Context())
	if err := s.lb.Failover(r.Context(), req.Region, user.Username); err != nil {
		s.fail(w, "failover", err)
		return
	}
	s.audit(r, "lb.failover", req.Region, nil)

	// A manual failover is itself an incident worth recording — it is the
	// operator declaring a confirmed regional problem.
	if err := s.incidents.Page(r.Context(), models.SeverityCritical, req.Region,
		"Manual failover executed",
		"All traffic routed to "+req.Region+" by "+user.Username+
			". Other regions pulled from rotation; on-call paged in both regions.",
	); err != nil {
		s.log.Error("record failover incident", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"region": req.Region})
}

// ----------------------------------------------------------- rollouts

func (s *Server) handleRolloutPause(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if err := s.rollouts.Pause(r.Context(), id, user.Username); err != nil {
		s.fail(w, "pause rollout", err)
		return
	}
	s.audit(r, "rollout.pause", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "paused": true})
}

func (s *Server) handleRolloutResume(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if err := s.rollouts.Resume(r.Context(), id, user.Username); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "rollout not found")
			return
		}
		// A refused promotion is a client-visible policy decision, not a
		// server fault — surface the reason verbatim.
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(r, "rollout.resume", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "paused": false})
}

func (s *Server) handleRolloutRollback(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if err := s.rollouts.Rollback(r.Context(), id, user.Username); err != nil {
		s.fail(w, "roll back rollout", err)
		return
	}
	s.audit(r, "rollout.rollback", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "rolledBack": true})
}

// -------------------------------------------------------------- admin

type createServiceRequest struct {
	Name                        string            `json:"name"`
	Runtime                     string            `json:"runtime"`
	Role                        string            `json:"role"`
	ProbeType                   string            `json:"probeType"`
	ProbeConfig                 map[string]any    `json:"probeConfig"`
	ProbeIntervalS              int               `json:"probeIntervalS"`
	ProbeTimeoutS               int               `json:"probeTimeoutS"`
	ConsecutiveFailuresToTrip   int               `json:"consecutiveFailuresToTrip"`
	ConsecutiveSuccessToRecover int               `json:"consecutiveSuccessToRecover"`
	CACertSecretEnv             string            `json:"caCertSecretEnv"`
	AWSAuthMethod               string            `json:"awsAuthMethod"`
	AWSAuthRef                  string            `json:"awsAuthRef"`
	Regions                     map[string]string `json:"regions"` // region id -> endpoint
	Versions                    map[string]string `json:"versions"`
}

func (s *Server) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	svc, err := req.toModel()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	created, err := s.store.CreateService(r.Context(), svc)
	if err != nil {
		s.fail(w, "create service", err)
		return
	}
	s.audit(r, "service.create", created.Name, map[string]any{
		"probeType": created.ProbeType, "role": created.Role,
	})

	// Start probing immediately rather than waiting for the next restart.
	s.syncServices(r)

	writeJSON(w, http.StatusCreated, created)
}

type updateRoleRequest struct {
	Role string `json:"role"`
}

func (s *Server) handleUpdateServiceRole(w http.ResponseWriter, r *http.Request) {
	var req updateRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	role := models.ProbeRole(req.Role)
	if role != models.RoleHealthcheckOnly && role != models.RoleHealthcheckFailover {
		writeError(w, http.StatusBadRequest,
			"role must be healthcheck_only or healthcheck_failover")
		return
	}

	id := r.PathValue("id")
	if err := s.store.UpdateServiceRole(r.Context(), id, role); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		s.fail(w, "update service role", err)
		return
	}
	s.audit(r, "service.update_role", id, map[string]any{"role": role})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "role": role})
}

func (s *Server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteService(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		s.fail(w, "delete service", err)
		return
	}
	s.audit(r, "service.delete", id, nil)
	s.syncServices(r)
	writeJSON(w, http.StatusNoContent, nil)
}

type upsertRegionRequest struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Provider    string `json:"provider"`
	ZoneLabel   string `json:"zoneLabel"`
}

func (s *Server) handleUpsertRegion(w http.ResponseWriter, r *http.Request) {
	var req upsertRegionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.ID
	}
	if req.Provider == "" {
		req.Provider = "AWS"
	}

	region := models.Region{ID: req.ID, DisplayName: req.DisplayName, Provider: req.Provider}
	if err := s.store.UpsertRegion(r.Context(), region, req.ZoneLabel); err != nil {
		s.fail(w, "upsert region", err)
		return
	}
	s.audit(r, "region.upsert", req.ID, nil)
	writeJSON(w, http.StatusOK, region)
}

func (s *Server) handleDeleteRegion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteRegion(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "region not found")
			return
		}
		s.fail(w, "delete region", err)
		return
	}
	s.audit(r, "region.delete", id, nil)
	s.syncServices(r)
	writeJSON(w, http.StatusNoContent, nil)
}

type upsertRoutingRequest struct {
	Enabled    bool           `json:"enabled"`
	Config     map[string]any `json:"config"`
	AuthMethod string         `json:"authMethod"`
	AuthRef    string         `json:"authRef"`
}

// handleUpsertRouting saves a provider config.
//
// It validates by *building* the adapter, which resolves the credential
// reference and checks required config fields. Saving a config that cannot
// actually authenticate is how you discover the problem during an incident
// instead of at configuration time.
func (s *Server) handleUpsertRouting(w http.ResponseWriter, r *http.Request) {
	var req upsertRoutingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	provider := models.RoutingProvider(r.PathValue("provider"))
	switch provider {
	case models.ProviderAWSNative, models.ProviderCloudFront,
		models.ProviderAviGSLB, models.ProviderAkamaiCloudlet:
	default:
		writeError(w, http.StatusBadRequest, "unknown routing provider")
		return
	}

	ep := models.RoutingEndpoint{
		Provider:   provider,
		Enabled:    req.Enabled,
		Config:     req.Config,
		AuthMethod: req.AuthMethod,
		AuthRef:    req.AuthRef,
	}
	if ep.Config == nil {
		ep.Config = map[string]any{}
	}

	if ep.Enabled {
		if _, err := s.registry.Build(r.Context(), ep); err != nil {
			writeError(w, http.StatusBadRequest, "configuration rejected: "+err.Error())
			return
		}
	}

	if err := s.store.UpsertRoutingEndpoint(r.Context(), ep); err != nil {
		s.fail(w, "save routing endpoint", err)
		return
	}
	s.audit(r, "routing.upsert", string(provider), map[string]any{"enabled": req.Enabled})
	writeJSON(w, http.StatusOK, ep)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.store.ListAudit(r.Context(), limit)
	if err != nil {
		s.fail(w, "list audit log", err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ------------------------------------------------------------ helpers

// toModel validates and converts a create request.
func (req createServiceRequest) toModel() (models.Service, error) {
	var svc models.Service

	if req.Name == "" {
		return svc, errors.New("name is required")
	}
	if len(req.Regions) == 0 {
		return svc, errors.New("at least one region is required")
	}

	role := models.ProbeRole(req.Role)
	if role == "" {
		// Default to the safe option: a newly registered service is observed
		// but cannot move traffic until someone opts in.
		role = models.RoleHealthcheckOnly
	}
	if role != models.RoleHealthcheckOnly && role != models.RoleHealthcheckFailover {
		return svc, errors.New("role must be healthcheck_only or healthcheck_failover")
	}

	probeType := models.ProbeType(req.ProbeType)
	switch probeType {
	case models.ProbeHTTP, models.ProbeTCP, models.ProbeALB,
		models.ProbeNLB, models.ProbeGRPC, models.ProbeCustom:
	case "":
		probeType = models.ProbeHTTP
	default:
		return svc, errors.New("unsupported probeType")
	}

	authMethod := models.AWSAuthMethod(req.AWSAuthMethod)
	switch authMethod {
	case models.AuthAssumeRole, models.AuthServiceAccount,
		models.AuthAWSSSO, models.AuthCustomSSOBinary, "":
	default:
		return svc, errors.New("unsupported awsAuthMethod")
	}
	if (probeType == models.ProbeALB || probeType == models.ProbeNLB) && authMethod == "" {
		return svc, errors.New("ALB/NLB probes require an awsAuthMethod")
	}

	svc = models.Service{
		Name:                        req.Name,
		Runtime:                     req.Runtime,
		Role:                        role,
		ProbeType:                   probeType,
		ProbeConfig:                 req.ProbeConfig,
		ProbeIntervalS:              orDefault(req.ProbeIntervalS, 5),
		ProbeTimeoutS:               orDefault(req.ProbeTimeoutS, 3),
		ConsecutiveFailuresToTrip:   orDefault(req.ConsecutiveFailuresToTrip, 3),
		ConsecutiveSuccessToRecover: orDefault(req.ConsecutiveSuccessToRecover, 2),
		CACertSecretEnv:             req.CACertSecretEnv,
		AWSAuthMethod:               authMethod,
		AWSAuthRef:                  req.AWSAuthRef,
	}
	if svc.ProbeConfig == nil {
		svc.ProbeConfig = map[string]any{}
	}
	if svc.ProbeTimeoutS > svc.ProbeIntervalS {
		return svc, errors.New("probeTimeoutS must not exceed probeIntervalS")
	}

	for regionID, endpoint := range req.Regions {
		svc.Regions = append(svc.Regions, models.ServiceRegion{
			RegionID: regionID,
			Endpoint: endpoint,
			Version:  req.Versions[regionID],
		})
	}
	return svc, nil
}

// syncServices asks the prober to reconcile its targets after a change.
func (s *Server) syncServices(r *http.Request) {
	if s.syncer == nil {
		return
	}
	if err := s.syncer.SyncNow(r.Context()); err != nil {
		s.log.Error("sync probe targets", "error", err)
	}
}

// audit records an admin action, best-effort.
func (s *Server) audit(r *http.Request, action, target string, detail map[string]any) {
	user, _ := auth.UserFrom(r.Context())
	if err := s.store.Audit(r.Context(), user.Username, action, target, detail); err != nil {
		s.log.Error("write audit log", "action", action, "error", err)
	}
}

// fail logs an internal error and returns a generic message, so database
// details never reach the client.
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	s.log.Error(op, "error", err)
	writeError(w, http.StatusInternalServerError, op+" failed")
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
