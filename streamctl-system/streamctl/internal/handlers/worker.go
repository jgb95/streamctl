package handlers

import (
	"log"
	"net/http"
)

func (h *Handler) worker(w http.ResponseWriter, r *http.Request) {
	worker := h.gpuWorkerView(r.Context())
	status := h.gpuStatus(r.Context(), worker)
	openQueue, err := h.DB.ListOpenGPUQueueItems(1000)
	if err != nil {
		log.Printf("listing GPU queue failed: %v", err)
	}
	openQueue = h.reconcileStaleGPUQueue(worker, status.Jobs, openQueue)
	nowProcessing := currentLivestreamGPUJob(status.Jobs)
	normalizeQueue, queueErr := h.gpuQueueDashboard(status.Jobs, nowProcessing)
	if queueErr != nil {
		log.Printf("loading GPU queue dashboard failed: %v", queueErr)
	}
	queuedCount, runningCount := countGPUQueueStates(openQueue)
	renderQueue := h.renderQueueView()
	h.render(w, r, "worker.html", map[string]any{
		"GPUConfigured": worker.Configured, "GPUWorker": worker, "GPUStatus": status,
		"GPUAvailability": h.gpuAvailability(r.Context()), "NormalizeQueue": normalizeQueue, "RenderQueue": renderQueue,
		"QueuedCount": queuedCount + renderQueue.Queued, "RunningCount": runningCount + renderQueue.Running,
		"QueueNeedsWorker": queuedCount+renderQueue.Queued > 0 && worker.Managed && worker.Status != "active",
		"Requeued":         r.URL.Query().Get("requeued"), "RequeuedStale": r.URL.Query().Get("requeued_stale"),
		"ClearedFailures": r.URL.Query().Get("cleared_failures"),
	})
}
