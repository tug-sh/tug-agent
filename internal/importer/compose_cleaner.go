package importer

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"

	"tug.sh/pkg/protocol"
)

var (
	// hostRuleRe matches Traefik Host() rules, e.g.:
	// Host(`example.com`) or Host("example.com", "www.example.com") or Host(`api.test.dev`)
	hostRuleRe = regexp.MustCompile(`Host\(([^)]+)\)`)

	// domainExtractRe matches individual domain names inside Host(...) rule
	domainExtractRe = regexp.MustCompile("['`\"]([a-zA-Z0-9.-]+)['`\"]")

	// portLabelRe matches loadbalancer server port label, e.g.
	// traefik.http.services.app.loadbalancer.server.port=8080
	portLabelRe = regexp.MustCompile(`(?:loadbalancer\.server\.port|port)[:=\s]+["']?([0-9]+)["']?`)
)

// CleanedComposeResult contains the sanitized compose YAML and any domains/ports extracted.
type CleanedComposeResult struct {
	CleanedYAML string
	Routes      []protocol.ImportDomainRoute
}

// ExtractRoutesAndCleanCompose parses compose YAML text, pulls out Traefik domain routes
// and target ports, and strips away third-party panel/proxy labels (Traefik, Coolify).
func ExtractRoutesAndCleanCompose(rawYAML string) CleanedComposeResult {
	var routes []protocol.ImportDomainRoute
	routeByDomain := make(map[string]protocol.ImportDomainRoute)

	var cleanedLines []string
	scanner := bufio.NewScanner(strings.NewReader(rawYAML))

	currentService := ""
	serviceRe := regexp.MustCompile(`^[ \t]{2}([a-zA-Z0-9_-]+):[ \t]*$`)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Check service name indentation (2 spaces in standard compose)
		if sm := serviceRe.FindStringSubmatch(line); len(sm) == 2 && !strings.HasPrefix(trimmed, "volumes:") && !strings.HasPrefix(trimmed, "networks:") {
			currentService = sm[1]
		}

		// Check if line contains a Traefik router Host() rule
		if strings.Contains(line, "traefik.http.routers") && strings.Contains(line, "rule") {
			if hm := hostRuleRe.FindStringSubmatch(line); len(hm) > 1 {
				domains := domainExtractRe.FindAllStringSubmatch(hm[1], -1)
				for _, dm := range domains {
					if len(dm) > 1 {
						dom := strings.ToLower(strings.TrimSpace(dm[1]))
						if dom != "" {
							r := routeByDomain[dom]
							r.Domain = dom
							r.TargetService = currentService
							r.HTTPS = true
							routeByDomain[dom] = r
						}
					}
				}
			}
		}

		// Check if line contains Traefik port specification
		if strings.Contains(line, "loadbalancer.server.port") {
			if pm := portLabelRe.FindStringSubmatch(line); len(pm) > 1 {
				if port, err := strconv.Atoi(pm[1]); err == nil && port > 0 {
					for dom, r := range routeByDomain {
						if r.TargetService == currentService && r.TargetPort == 0 {
							r.TargetPort = port
							routeByDomain[dom] = r
						}
					}
				}
			}
		}

		// Filter out Traefik and Coolify specific labels
		if isLabelToStrip(trimmed) {
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	for _, r := range routeByDomain {
		routes = append(routes, r)
	}

	return CleanedComposeResult{
		CleanedYAML: strings.TrimSpace(strings.Join(cleanedLines, "\n")),
		Routes:      routes,
	}
}

// isLabelToStrip returns true if the line is a label specific to Dokploy/Coolify/Traefik.
func isLabelToStrip(line string) bool {
	lower := strings.ToLower(line)
	// Strip lines with traefik.* or coolify.* labels
	if strings.Contains(lower, "traefik.") ||
		strings.Contains(lower, "coolify.managed") ||
		strings.Contains(lower, "coolify.type") ||
		strings.Contains(lower, "coolify.name") ||
		strings.Contains(lower, "coolify.applicationid") ||
		strings.Contains(lower, "coolify.serviceid") {
		return true
	}
	return false
}

// ParseEnvFile parses raw .env content into a key-value map.
func ParseEnvFile(content string) map[string]string {
	env := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eqIdx])
		val := strings.TrimSpace(line[eqIdx+1:])
		// Remove surrounding single or double quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" {
			env[key] = val
		}
	}
	return env
}
