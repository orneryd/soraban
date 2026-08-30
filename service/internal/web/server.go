package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"readiness.local/postgres/lifecycle"
)

type Application interface {
	Import(context.Context, string, int, string) (lifecycle.DatasetResult, error)
	DetermineAndPlan(context.Context, string, int64) (lifecycle.DeterminationResult, lifecycle.BatchPlanResult, error)
	FirmStatus(context.Context, string) (lifecycle.FirmStatus, error)
	ClientStatus(context.Context, string, string) (lifecycle.ClientStatus, error)
	Exceptions(context.Context, string, string) ([]lifecycle.ExceptionGroup, error)
}

type WorkerRunner interface {
	Run(context.Context, []string) error
}

type WorkerController struct {
	mu      sync.Mutex
	runner  WorkerRunner
	firms   []string
	cancel  context.CancelFunc
	running bool
}

func NewWorkerController(runner WorkerRunner, firms []string) *WorkerController {
	return &WorkerController{runner: runner, firms: append([]string(nil), firms...)}
}

func (controller *WorkerController) Start(parent context.Context) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.running || controller.runner == nil || len(controller.firms) == 0 {
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	controller.cancel = cancel
	controller.running = true
	go func() {
		_ = controller.runner.Run(ctx, controller.firms)
		controller.mu.Lock()
		controller.running = false
		controller.cancel = nil
		controller.mu.Unlock()
	}()
	return true
}

func (controller *WorkerController) Stop() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !controller.running || controller.cancel == nil {
		return false
	}
	controller.cancel()
	return true
}

func (controller *WorkerController) Running() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.running
}

type Server struct {
	application Application
	firms       []string
	workers     *WorkerController
	template    *template.Template
	mux         *http.ServeMux
}

type pageData struct {
	Title      string
	Message    string
	Error      string
	WorkerRuns bool
	Firms      []lifecycle.FirmStatus
	Client     *lifecycle.ClientStatus
	Exceptions []lifecycle.ExceptionGroup
}

func New(application Application, firms []string, workers *WorkerController) (*Server, error) {
	if application == nil || len(firms) == 0 {
		return nil, errors.New("web server requires application and firms")
	}
	parsed, err := template.New("page").Parse(pageTemplate)
	if err != nil {
		return nil, err
	}
	server := &Server{application: application, firms: append([]string(nil), firms...), workers: workers, template: parsed, mux: http.NewServeMux()}
	server.mux.HandleFunc("GET /healthz", server.health)
	server.mux.HandleFunc("GET /", server.home)
	server.mux.HandleFunc("GET /firms/{firm}/clients/{client}", server.client)
	server.mux.HandleFunc("POST /actions/import", server.importAction)
	server.mux.HandleFunc("POST /actions/determine", server.determineAction)
	server.mux.HandleFunc("POST /actions/workers/start", server.startWorkers)
	server.mux.HandleFunc("POST /actions/workers/stop", server.stopWorkers)
	return server, nil
}

func (server *Server) Handler() http.Handler { return server.mux }

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = writer.Write([]byte("ok\n"))
}

func (server *Server) home(writer http.ResponseWriter, request *http.Request) {
	data := pageData{Title: "Filing operations", Message: request.URL.Query().Get("message")}
	if server.workers != nil {
		data.WorkerRuns = server.workers.Running()
	}
	for _, firmID := range server.firms {
		status, err := server.application.FirmStatus(request.Context(), firmID)
		if errors.Is(err, lifecycle.ErrNotFound) {
			continue
		}
		if err != nil {
			data.Error = "Unable to load committed filing status."
			break
		}
		data.Firms = append(data.Firms, status)
	}
	server.render(writer, data)
}

func (server *Server) client(writer http.ResponseWriter, request *http.Request) {
	firmID, clientID := request.PathValue("firm"), request.PathValue("client")
	status, err := server.application.ClientStatus(request.Context(), firmID, clientID)
	if err != nil {
		http.Error(writer, "client status unavailable", http.StatusNotFound)
		return
	}
	exceptions, err := server.application.Exceptions(request.Context(), firmID, clientID)
	if err != nil {
		http.Error(writer, "exceptions unavailable", http.StatusInternalServerError)
		return
	}
	server.render(writer, pageData{Title: firmID + " / " + clientID, Client: &status, Exceptions: exceptions})
}

func (server *Server) importAction(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	taxYear, err := strconv.Atoi(request.FormValue("tax_year"))
	if err != nil {
		http.Error(writer, "invalid tax year", http.StatusBadRequest)
		return
	}
	result, err := server.application.Import(request.Context(), request.FormValue("firm_id"), taxYear, request.FormValue("input"))
	if err != nil {
		http.Error(writer, "import failed", http.StatusUnprocessableEntity)
		return
	}
	redirect(writer, request, fmt.Sprintf("dataset %d imported (%d rows)", result.DatasetID, result.RowCount))
}

func (server *Server) determineAction(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	datasetID, err := strconv.ParseInt(request.FormValue("dataset_id"), 10, 64)
	if err != nil || datasetID <= 0 {
		http.Error(writer, "invalid dataset", http.StatusBadRequest)
		return
	}
	determination, plan, err := server.application.DetermineAndPlan(request.Context(), request.FormValue("firm_id"), datasetID)
	if err != nil {
		http.Error(writer, "determination failed", http.StatusUnprocessableEntity)
		return
	}
	redirect(writer, request, fmt.Sprintf("determination %d ready; %d batches created", determination.DeterminationID, plan.CreatedBatchCount))
}

func (server *Server) startWorkers(writer http.ResponseWriter, request *http.Request) {
	if server.workers == nil || !server.workers.Start(context.Background()) {
		redirect(writer, request, "workers already running")
		return
	}
	redirect(writer, request, "workers started")
}

func (server *Server) stopWorkers(writer http.ResponseWriter, request *http.Request) {
	if server.workers == nil || !server.workers.Stop() {
		redirect(writer, request, "workers already stopped")
		return
	}
	redirect(writer, request, "workers stopping")
}

func redirect(writer http.ResponseWriter, request *http.Request, message string) {
	http.Redirect(writer, request, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func (server *Server) render(writer http.ResponseWriter, data pageData) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := server.template.Execute(writer, data); err != nil {
		http.Error(writer, "render failed", http.StatusInternalServerError)
	}
}

const pageTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>
:root{--ink:#18211b;--muted:#5f6b63;--paper:#f5f7f2;--panel:#fff;--line:#cbd2ca;--green:#23623b;--amber:#925f0a;--red:#963e35}*{box-sizing:border-box}body{margin:0;background:linear-gradient(135deg,#edf2e8,#f8f4e9);color:var(--ink);font-family:Georgia,serif}header,main{max-width:1180px;margin:auto;padding:24px}header{display:flex;align-items:center;justify-content:space-between;border-bottom:1px solid var(--line)}h1,h2{letter-spacing:0;margin:0 0 16px}h1{font-size:30px}.muted{color:var(--muted)}.toolbar,.grid,.counts{display:grid;gap:12px}.toolbar{grid-template-columns:repeat(3,minmax(0,1fr));margin:24px 0}.grid{grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}.counts{grid-template-columns:repeat(3,minmax(0,1fr));margin:14px 0}section,article{min-width:0;background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:18px}form{display:grid;min-width:0;gap:10px}input,select,button{min-width:0;width:100%;font:inherit;padding:9px;border:1px solid var(--line);border-radius:4px}button{background:var(--green);color:#fff;border:0;cursor:pointer}.count{min-width:0}.count strong{display:block;font:700 22px ui-monospace,monospace}.count span{font-size:14px;color:var(--muted)}.status{font:700 13px ui-monospace,monospace;text-transform:uppercase}.needs_attention{color:var(--red)}.awaiting_the_irs{color:var(--amber)}.fully_filed{color:var(--green)}a{color:var(--green)}table{width:100%;border-collapse:collapse}td,th{text-align:left;padding:8px;border-bottom:1px solid var(--line)}@media(max-width:760px){.toolbar{grid-template-columns:1fr}.grid{grid-template-columns:1fr}header{align-items:flex-start;gap:12px}}
</style></head><body><header><div><h1>{{.Title}}</h1><div class="muted">Durable 1099-NEC filing operations</div></div>{{if .WorkerRuns}}<span class="status awaiting_the_irs">Workers running</span>{{end}}</header><main>
{{if .Message}}<p>{{.Message}}</p>{{end}}{{if .Error}}<p class="needs_attention">{{.Error}}</p>{{end}}
{{if .Client}}<section><div class="status {{.Client.Headline}}">{{.Client.Headline}}</div>{{template "counts" .Client.Counts}}</section>{{range .Exceptions}}<section><h2>{{.Type}} ({{.Count}})</h2><table><tr><th>Client</th><th>Vendor</th><th>Reason</th></tr>{{range .Items}}<tr><td>{{.ClientID}}</td><td>{{.VendorDisplayName}}</td><td>{{.FailureCode}}</td></tr>{{end}}</table></section>{{end}}{{else}}
<div class="toolbar"><section><h2>Import</h2><form method="post" action="/actions/import"><input name="firm_id" placeholder="Firm ID" required><input name="tax_year" value="2025" required><input name="input" value="data/firm_F001_export.csv.gz" required><button>Import export</button></form></section><section><h2>Determine</h2><form method="post" action="/actions/determine"><input name="firm_id" placeholder="Firm ID" required><input name="dataset_id" placeholder="Dataset ID" required><button>Determine and plan</button></form></section><section><h2>Workers</h2><form method="post" action="/actions/workers/start"><button>Start workers</button></form><form method="post" action="/actions/workers/stop"><button>Stop workers</button></form></section></div>
<div class="grid">{{range .Firms}}<article><h2>{{.FirmID}}</h2><div class="status {{.Headline}}">{{.Headline}}</div>{{template "counts" .Counts}}<table><tr><th>Client</th><th>Status</th></tr>{{range .Clients}}<tr><td><a href="/firms/{{.FirmID}}/clients/{{.ClientID}}">{{.ClientID}}</a></td><td class="status {{.Headline}}">{{.Headline}}</td></tr>{{end}}</table></article>{{end}}</div>{{end}}</main></body></html>
{{define "counts"}}<div class="counts"><div class="count"><strong>{{.Required}}</strong><span>Required</span></div><div class="count"><strong>{{.Blocked}}</strong><span>Blocked</span></div><div class="count"><strong>{{.Ready}}</strong><span>Ready</span></div><div class="count"><strong>{{.Pending}}</strong><span>Pending</span></div><div class="count"><strong>{{.Accepted}}</strong><span>Accepted</span></div><div class="count"><strong>{{.Rejected}}</strong><span>Rejected</span></div></div>{{end}}`
