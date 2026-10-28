package agent

import (
	"encoding/json"
	"fmt"

	"tug.sh/pkg/protocol"
)

func (runtime *Runtime) handleImportScan(request commandRequest) ([]string, error) {
	if runtime.importer == nil {
		return nil, fmt.Errorf("importer subsystem is not initialized")
	}

	result, err := runtime.importer.Scan(request.ctx, request.command.ImportSource)
	if err != nil {
		return nil, err
	}

	request.setPayload(result)
	return []string{
		fmt.Sprintf("Workload scan completed. Source: %s (version: %s). Detected %d projects.",
			result.DetectedSource, result.DetectedVersion, len(result.Projects)),
	}, nil
}

func (runtime *Runtime) handleImportApply(request commandRequest) ([]string, error) {
	if runtime.importer == nil {
		return nil, fmt.Errorf("importer subsystem is not initialized")
	}

	var applyReq protocol.ImportApplyRequest
	if request.command.ImportPayload != "" {
		if err := json.Unmarshal([]byte(request.command.ImportPayload), &applyReq); err != nil {
			return nil, fmt.Errorf("invalid import payload: %w", err)
		}
	}

	result, err := runtime.importer.Apply(request.ctx, applyReq)
	if err != nil {
		return nil, err
	}

	request.setPayload(result)

	logs := []string{
		fmt.Sprintf("Import applied: %d projects materialized, %d routes configured.",
			len(result.ImportedProjects), len(result.ConfiguredRoutes)),
	}
	if result.StoppedProxy {
		logs = append(logs, "Stopped conflicting legacy proxy container.")
	}
	if len(result.Errors) > 0 {
		logs = append(logs, fmt.Sprintf("Encountered %d warning(s) during apply.", len(result.Errors)))
	}

	return logs, nil
}
