package main

import (
	"context"
	"errors"
	"strings"
)

type registeredTargetConnectionTestInput struct {
	Adapter string `json:"adapter"`
	Target  string `json:"target"`
}

type registeredTargetConnectionTestResult struct {
	Adapter     string `json:"adapter"`
	Target      string `json:"target"`
	Result      string `json:"result"`
	CompletedAt string `json:"completed_at,omitempty"`
	Message     string `json:"message"`
}

func (a *app) testRegisteredTargetConnection(ctx context.Context, input registeredTargetConnectionTestInput) (registeredTargetConnectionTestResult, error) {
	if input.Target == "" || len(input.Target) > 80 || strings.ContainsAny(input.Target, "\r\n\x00") {
		return registeredTargetConnectionTestResult{}, errors.New("An exact registered target name is required.")
	}
	targetID, err := a.registeredConnectionTestTargetID(input.Adapter, input.Target)
	if err != nil {
		return registeredTargetConnectionTestResult{}, err
	}

	attempt := a.testConnection(ctx, input.Adapter, targetID, input.Target)
	result := registeredTargetConnectionTestResult{
		Adapter: input.Adapter,
		Target:  input.Target,
		Result:  "not_tested",
	}
	result.Message, _ = connectionTestMessage(attempt)
	if attempt.attempted {
		result.Result = "failure"
		if attempt.succeeded {
			result.Result = "success"
		}
		if attempt.historySaved {
			result.CompletedAt = attempt.completedAt
		}
	}
	return result, nil
}

func (a *app) registeredConnectionTestTargetID(adapter, name string) (string, error) {
	switch adapter {
	case "github", "jenkins", "harbor", "dashboard":
	default:
		return "", errors.New("Choose one supported registered target adapter.")
	}

	var targetID string
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local target settings are invalid.")
		}
		unavailable := errors.New("The requested registered target is unavailable or invalid.")
		switch adapter {
		case "github":
			index := findGitHubTargetNameIndex(cfg.GitHubTargets, name)
			if index < 0 || cfg.GitHubTargets[index].Disabled || validateTarget(cfg.GitHubTargets[index]) != nil {
				return unavailable
			}
			targetID = cfg.GitHubTargets[index].ID
		case "jenkins":
			index := findJenkinsTargetNameIndex(cfg.JenkinsTargets, name)
			if index < 0 || cfg.JenkinsTargets[index].Disabled || validateJenkinsTarget(cfg.JenkinsTargets[index]) != nil {
				return unavailable
			}
			targetID = cfg.JenkinsTargets[index].ID
		case "harbor":
			index := findHarborTargetNameIndex(cfg.HarborTargets, name)
			if index < 0 || cfg.HarborTargets[index].Disabled || validateHarborTarget(cfg.HarborTargets[index]) != nil {
				return unavailable
			}
			targetID = cfg.HarborTargets[index].ID
		case "dashboard":
			index := findDashboardTargetNameIndex(cfg.DashboardTargets, name)
			if index < 0 || cfg.DashboardTargets[index].Disabled || validateDashboardTarget(cfg.DashboardTargets[index]) != nil {
				return unavailable
			}
			targetID = cfg.DashboardTargets[index].ID
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return targetID, nil
}
