package api

import (
	"encoding/json"
	"net/http"
	"regexp"

	"guarde.sh/nala/internal/store"
)

type graphNode struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	jobSpec
}

type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	On   string `json:"on"`
}

type graph struct {
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

const (
	maxNodes = 50
	maxEdges = 200
)

var nodeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func (g *graph) validate() error {
	if len(g.Nodes) == 0 {
		return errBadRequest("a workflow needs at least one job")
	}
	if len(g.Nodes) > maxNodes {
		return errBadRequest("a workflow may have at most %d jobs", maxNodes)
	}
	if len(g.Edges) > maxEdges {
		return errBadRequest("a workflow may have at most %d connections", maxEdges)
	}
	index := map[string]int{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !nodeIDRe.MatchString(n.ID) {
			return errBadRequest("job id %q is invalid", n.ID)
		}
		if _, dup := index[n.ID]; dup {
			return errBadRequest("job id %q is used twice", n.ID)
		}
		index[n.ID] = i
		if err := n.checkShape(); err != nil {
			label := n.Name
			if label == "" {
				label = n.ID
			}
			return errBadRequest("job %q: %v", label, err)
		}
	}
	indegree := make([]int, len(g.Nodes))
	children := make([][]int, len(g.Nodes))
	seen := map[[2]string]bool{}
	for _, e := range g.Edges {
		from, okFrom := index[e.From]
		to, okTo := index[e.To]
		switch {
		case !okFrom || !okTo:
			return errBadRequest("a connection points at a job that does not exist")
		case from == to:
			return errBadRequest("a job cannot depend on itself")
		case seen[[2]string{e.From, e.To}]:
			return errBadRequest("two jobs can only be connected once")
		case e.On != "success" && e.On != "failure" && e.On != "always":
			return errBadRequest(`a connection's condition must be "success", "failure" or "always"`)
		}
		seen[[2]string{e.From, e.To}] = true
		indegree[to]++
		children[from] = append(children[from], to)
	}
	var ready []int
	for i, d := range indegree {
		if d == 0 {
			ready = append(ready, i)
		}
	}
	removed := 0
	for len(ready) > 0 {
		i := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		removed++
		for _, c := range children[i] {
			if indegree[c]--; indegree[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if removed != len(g.Nodes) {
		return errBadRequest("the connections form a loop, so those jobs could never start")
	}
	return nil
}

type workflowBody struct {
	Name  string `json:"name"`
	Graph graph  `json:"graph"`
}

func readWorkflow(r *http.Request) (name string, g graph, raw []byte, err error) {
	var in workflowBody
	if err = readJSON(r, &in); err != nil {
		return
	}
	if name, err = validName("name", in.Name); err != nil {
		return
	}
	if err = in.Graph.validate(); err != nil {
		return
	}
	raw, err = json.Marshal(in.Graph)
	return name, in.Graph, raw, err
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) error {
	wfs, err := s.store.ListWorkflows(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"workflows": wfs})
	return nil
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) error {
	name, _, raw, err := readWorkflow(r)
	if err != nil {
		return err
	}
	wf, err := s.store.CreateWorkflow(r.Context(), orgIDOf(r), name, raw, principalOf(r).actor())
	if err != nil {
		return err
	}
	s.auditOrg(r, "workflow.create", wf.ID, map[string]string{"name": name})
	writeJSON(w, http.StatusCreated, map[string]any{"workflow": wf})
	return nil
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "workflow")
	if err != nil {
		return err
	}
	wf, err := s.store.GetWorkflow(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"workflow": wf})
	return nil
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "workflow")
	if err != nil {
		return err
	}
	name, _, raw, err := readWorkflow(r)
	if err != nil {
		return err
	}
	wf, err := s.store.UpdateWorkflow(r.Context(), orgIDOf(r), id, name, raw)
	if err != nil {
		return err
	}
	s.auditOrg(r, "workflow.update", id, nil)
	writeJSON(w, 200, map[string]any{"workflow": wf})
	return nil
}

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "workflow")
	if err != nil {
		return err
	}
	if err := s.store.DeleteWorkflow(r.Context(), orgIDOf(r), id); err != nil {
		return err
	}
	s.auditOrg(r, "workflow.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleRunWorkflow(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "workflow")
	if err != nil {
		return err
	}
	wf, err := s.store.GetWorkflow(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	var g graph
	if err := json.Unmarshal(wf.Graph, &g); err != nil {
		return err
	}
	if err := g.validate(); err != nil {
		return err
	}
	org, err := s.store.GetOrg(r.Context(), orgIDOf(r))
	if err != nil {
		return err
	}
	actor := principalOf(r).actor()
	nodes := make([]store.NewJob, len(g.Nodes))
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if err := s.checkPolicy(r, org, &n.jobSpec); err != nil {
			if ae, ok := err.(*apiError); ok {
				label := n.Name
				if label == "" {
					label = n.ID
				}
				return &apiError{ae.Status, ae.Code, `job "` + label + `": ` + ae.Message}
			}
			return err
		}
		nodes[i] = n.newJob(org.ID, actor)
		nodes[i].NodeID = n.ID
	}
	if err := s.checkQuota(r, org, len(nodes)); err != nil {
		return err
	}
	edges := make([]store.RunEdge, len(g.Edges))
	for i, e := range g.Edges {
		edges[i] = store.RunEdge{From: e.From, To: e.To, On: e.On}
	}
	run, err := s.store.StartRun(r.Context(), org.ID, &wf.ID, wf.Name, wf.Graph, actor, nodes, edges)
	if err != nil {
		return err
	}
	s.auditOrg(r, "workflow.run", run.ID, map[string]string{"workflow": wf.ID})
	writeJSON(w, http.StatusCreated, map[string]any{"run": run})
	return nil
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) error {
	wf := r.URL.Query().Get("workflow")
	if wf != "" && !uuidRe.MatchString(wf) {
		return errBadRequest("invalid workflow id")
	}
	runs, err := s.store.ListRuns(r.Context(), orgIDOf(r), wf, 50)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"runs": runs})
	return nil
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "run")
	if err != nil {
		return err
	}
	run, err := s.store.GetRun(r.Context(), orgIDOf(r), id)
	if err != nil {
		return err
	}
	orgID := orgIDOf(r)
	jobs, err := s.store.ListJobs(r.Context(), store.JobFilter{OrgID: &orgID, RunID: id, Limit: maxNodes})
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"run": run, "jobs": jobs})
	return nil
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "run")
	if err != nil {
		return err
	}
	if err := s.store.CancelRun(r.Context(), orgIDOf(r), id); err != nil {
		return err
	}
	s.auditOrg(r, "run.cancel", id, nil)
	w.WriteHeader(http.StatusAccepted)
	return nil
}
