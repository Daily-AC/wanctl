package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"wanctl/internal/agent"
	"wanctl/internal/release"
)

// Keep every source's diagnostic for wanctl logs, but do not send URLs, Go
// errors or stack details to the portal. A failed fallback must not erase an
// earlier unreachable source and turn uncertainty into "version not found".
func fetchPortalUpdate(target string, bases []string, attempt func(string) (updateFetch, error)) (updateFetch, error) {
	var failures []error
	allMissing, unexpected := len(bases) > 0, false
	unreachable := ""
	got, err := overSources(bases, func(base string) (updateFetch, error) {
		got, err := attempt(base)
		if err == nil {
			return got, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", base, err))
		var status *updateHTTPError
		var mismatch *release.ExactVersionMismatchError
		missing := errors.As(err, &mismatch)
		if errors.As(err, &status) && status.StatusCode == http.StatusNotFound && strings.HasSuffix(status.URL, "/"+release.ManifestName) {
			missing = true
		}
		if !missing {
			allMissing = false
			var network net.Error
			var request *url.Error
			if errors.As(err, &network) || errors.As(err, &request) || status != nil {
				if unreachable == "" {
					if u, e := url.Parse(base); e == nil {
						unreachable = u.Hostname()
					}
				}
			} else {
				unexpected = true
			}
		}
		return got, err
	})
	if err == nil {
		return got, nil
	}
	reason := "wanctl installation failed; check the device logs"
	if allMissing {
		reason = fmt.Sprintf("version %s was not found in the official releases", target)
	} else if !unexpected && unreachable != "" {
		reason = "the device could not reach " + unreachable
	}
	diagnostic := errors.Join(failures...)
	if diagnostic == nil {
		diagnostic = err
	}
	return updateFetch{}, &agent.InstallError{Reason: reason, Err: diagnostic}
}
