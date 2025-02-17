package main

import (
	"fmt"
	"github.com/diggerhq/digger/libs/comment_utils/reporting"
	"github.com/diggerhq/digger/libs/comment_utils/summary"
	core_locking "github.com/diggerhq/digger/libs/locking"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/diggerhq/digger/cli/pkg/azure"
	"github.com/diggerhq/digger/cli/pkg/bitbucket"
	core_backend "github.com/diggerhq/digger/cli/pkg/core/backend"
	core_policy "github.com/diggerhq/digger/cli/pkg/core/policy"
	core_storage "github.com/diggerhq/digger/cli/pkg/core/storage"
	"github.com/diggerhq/digger/cli/pkg/digger"
	"github.com/diggerhq/digger/cli/pkg/gitlab"
	"github.com/diggerhq/digger/cli/pkg/storage"
	"github.com/diggerhq/digger/cli/pkg/usage"
	"github.com/diggerhq/digger/libs/digger_config"
	orchestrator "github.com/diggerhq/digger/libs/orchestrator"
	"gopkg.in/yaml.v3"
)

func gitLabCI(lock core_locking.Lock, policyChecker core_policy.Checker, backendApi core_backend.Api, reportingStrategy reporting.ReportStrategy) {
	log.Println("Using GitLab.")

	projectNamespace := os.Getenv("CI_PROJECT_NAMESPACE")
	projectName := os.Getenv("CI_PROJECT_NAME")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		log.Println("GITLAB_TOKEN is empty")
	}

	currentDir, err := os.Getwd()
	if err != nil {
		usage.ReportErrorAndExit(projectNamespace, fmt.Sprintf("Failed to get current dir. %s", err), 4)
	}
	log.Printf("main: working dir: %s \n", currentDir)

	diggerConfig, diggerConfigYaml, dependencyGraph, err := digger_config.LoadDiggerConfig(currentDir, true)
	if err != nil {
		usage.ReportErrorAndExit(projectNamespace, fmt.Sprintf("Failed to read Digger digger_config. %s", err), 4)
	}
	log.Println("Digger digger_config read successfully")

	gitLabContext, err := gitlab.ParseGitLabContext()
	if err != nil {
		log.Printf("failed to parse GitLab context. %s\n", err.Error())
		os.Exit(4)
	}

	yamlData, err := yaml.Marshal(diggerConfigYaml)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	// Convert to string
	yamlStr := string(yamlData)
	repo := strings.ReplaceAll(gitLabContext.ProjectNamespace, "/", "-")

	for _, p := range diggerConfig.Projects {
		err = backendApi.ReportProject(repo, p.Name, yamlStr)
		if err != nil {
			log.Printf("Failed to report project %s. %s\n", p.Name, err)
		}
	}

	// it's ok to not have merge request info if it has been merged
	if (gitLabContext.MergeRequestIId == nil || len(gitLabContext.OpenMergeRequests) == 0) && gitLabContext.EventType != "merge_request_merge" {
		log.Println("No merge request found.")
		os.Exit(0)
	}

	gitlabService, err := gitlab.NewGitLabService(gitlabToken, gitLabContext)
	if err != nil {
		log.Printf("failed to initialise GitLab service, %v", err)
		os.Exit(4)
	}

	gitlabEvent := gitlab.GitLabEvent{EventType: gitLabContext.EventType}

	impactedProjects, requestedProject, err := gitlab.ProcessGitLabEvent(gitLabContext, diggerConfig, gitlabService)
	if err != nil {
		log.Printf("failed to process GitLab event, %v", err)
		os.Exit(6)
	}
	log.Println("GitLab event processed successfully")

	jobs, coversAllImpactedProjects, err := gitlab.ConvertGitLabEventToCommands(gitlabEvent, gitLabContext, impactedProjects, requestedProject, diggerConfig.Workflows)
	if err != nil {
		log.Printf("failed to convert event to command, %v", err)
		os.Exit(7)
	}
	log.Println("GitLab event converted to commands successfully")

	log.Println("Digger commands to be executed:")
	for _, v := range jobs {
		log.Printf("command: %s, project: %s\n", strings.Join(v.Commands, ", "), v.ProjectName)
	}

	planStorage := storage.NewPlanStorage("", "", "", gitLabContext.GitlabUserName, gitLabContext.MergeRequestIId)
	reporter := &reporting.CiReporter{
		CiService:      gitlabService,
		PrNumber:       *gitLabContext.MergeRequestIId,
		ReportStrategy: reportingStrategy,
	}
	jobs = digger.SortedCommandsByDependency(jobs, &dependencyGraph)
	allAppliesSuccess, atLeastOneApply, err := digger.RunJobs(jobs, gitlabService, gitlabService, lock, reporter, planStorage, policyChecker, comment_updater.NoopCommentUpdater{}, backendApi, "", false, false, 0, currentDir)

	if err != nil {
		log.Printf("failed to execute command, %v", err)
		os.Exit(8)
	}

	if diggerConfig.AutoMerge && atLeastOneApply && allAppliesSuccess && coversAllImpactedProjects {
		digger.MergePullRequest(gitlabService, *gitLabContext.MergeRequestIId)
		log.Println("Merge request changes has been applied successfully")
	}

	log.Println("Commands executed successfully")

	usage.ReportErrorAndExit(projectName, "Digger finished successfully", 0)
}

func azureCI(lock core_locking.Lock, policyChecker core_policy.Checker, backendApi core_backend.Api, reportingStrategy reporting.ReportStrategy) {
	log.Println("> Azure CI detected")
	azureContext := os.Getenv("AZURE_CONTEXT")
	azureToken := os.Getenv("AZURE_TOKEN")
	if azureToken == "" {
		log.Println("AZURE_TOKEN is empty")
	}
	parsedAzureContext, err := azure.GetAzureReposContext(azureContext)
	if err != nil {
		log.Printf("failed to parse Azure context. %s\n", err.Error())
		os.Exit(4)
	}

	currentDir, err := os.Getwd()
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to get current dir. %s", err), 4)
	}
	log.Printf("main: working dir: %s \n", currentDir)

	diggerConfig, diggerConfigYaml, dependencyGraph, err := digger_config.LoadDiggerConfig(currentDir, true)
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to read Digger digger_config. %s", err), 4)
	}
	log.Println("Digger digger_config read successfully")

	yamlData, err := yaml.Marshal(diggerConfigYaml)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	// Convert to string
	yamlStr := string(yamlData)
	repo := strings.ReplaceAll(parsedAzureContext.BaseUrl, "/", "-")

	for _, p := range diggerConfig.Projects {
		err = backendApi.ReportProject(repo, p.Name, yamlStr)
		if err != nil {
			log.Printf("Failed to report project %s. %s\n", p.Name, err)
		}
	}

	azureService, err := azure.NewAzureReposService(azureToken, parsedAzureContext.BaseUrl, parsedAzureContext.ProjectName, parsedAzureContext.RepositoryId)
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to initialise azure service. %s", err), 5)
	}

	impactedProjects, requestedProject, prNumber, err := azure.ProcessAzureReposEvent(parsedAzureContext.Event, diggerConfig, azureService)
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to process Azure event. %s", err), 6)
	}
	log.Println("Azure event processed successfully")

	jobs, coversAllImpactedProjects, err := azure.ConvertAzureEventToCommands(parsedAzureContext, impactedProjects, requestedProject, diggerConfig.Workflows)
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to convert event to command. %s", err), 7)

	}
	log.Println(fmt.Sprintf("Azure event converted to commands successfully: %v", jobs))

	for _, v := range jobs {
		log.Printf("command: %s, project: %s\n", strings.Join(v.Commands, ", "), v.ProjectName)
	}

	var planStorage core_storage.PlanStorage

	reporter := &reporting.CiReporter{
		CiService:      azureService,
		PrNumber:       prNumber,
		ReportStrategy: reportingStrategy,
	}
	jobs = digger.SortedCommandsByDependency(jobs, &dependencyGraph)
	allAppliesSuccess, atLeastOneApply, err := digger.RunJobs(jobs, azureService, azureService, lock, reporter, planStorage, policyChecker, comment_updater.NoopCommentUpdater{}, backendApi, "", false, false, 0, currentDir)
	if err != nil {
		usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, fmt.Sprintf("Failed to run commands. %s", err), 8)
	}

	if diggerConfig.AutoMerge && allAppliesSuccess && atLeastOneApply && coversAllImpactedProjects {
		digger.MergePullRequest(azureService, prNumber)
		log.Println("PR merged successfully")
	}

	log.Println("Commands executed successfully")

	usage.ReportErrorAndExit(parsedAzureContext.BaseUrl, "Digger finished successfully", 0)
}

func bitbucketCI(lock core_locking.Lock, policyChecker core_policy.Checker, backendApi core_backend.Api, reportingStrategy reporting.ReportStrategy) {
	log.Printf("Using Bitbucket.\n")
	actor := os.Getenv("BITBUCKET_STEP_TRIGGERER_UUID")
	if actor != "" {
		usage.SendUsageRecord(actor, "log", "initialize")
	} else {
		usage.SendUsageRecord("", "log", "non github initialisation")
	}

	runningMode := os.Getenv("INPUT_DIGGER_MODE")

	repository := os.Getenv("BITBUCKET_REPO_FULL_NAME")

	if repository == "" {
		usage.ReportErrorAndExit(actor, "BITBUCKET_REPO_FULL_NAME is not defined", 3)
	}

	splitRepositoryName := strings.Split(repository, "/")
	repoOwner, repositoryName := splitRepositoryName[0], splitRepositoryName[1]

	currentDir, err := os.Getwd()
	if err != nil {
		usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to get current dir. %s", err), 4)
	}

	diggerConfig, _, dependencyGraph, err := digger_config.LoadDiggerConfig("./", true)
	if err != nil {
		usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to read Digger digger_config. %s", err), 4)
	}
	log.Printf("Digger digger_config read successfully\n")

	authToken := os.Getenv("BITBUCKET_AUTH_TOKEN")

	if authToken == "" {
		usage.ReportErrorAndExit(actor, "BITBUCKET_AUTH_TOKEN is not defined", 3)
	}

	bitbucketService := bitbucket.BitbucketAPI{
		AuthToken:     authToken,
		HttpClient:    http.Client{},
		RepoWorkspace: repoOwner,
		RepoName:      repositoryName,
	}

	if runningMode == "manual" {
		command := os.Getenv("INPUT_DIGGER_COMMAND")
		if command == "" {
			usage.ReportErrorAndExit(actor, "provide 'command' to run in 'manual' mode", 1)
		}
		project := os.Getenv("INPUT_DIGGER_PROJECT")
		if project == "" {
			usage.ReportErrorAndExit(actor, "provide 'project' to run in 'manual' mode", 2)
		}

		var projectConfig digger_config.Project
		for _, projectConfig = range diggerConfig.Projects {
			if projectConfig.Name == project {
				break
			}
		}
		workflow := diggerConfig.Workflows[projectConfig.Workflow]

		stateEnvVars, commandEnvVars := digger_config.CollectTerraformEnvConfig(workflow.EnvVars)

		planStorage := storage.NewPlanStorage("", repoOwner, repositoryName, actor, nil)

		jobs := orchestrator.Job{
			ProjectName:       project,
			ProjectDir:        projectConfig.Dir,
			ProjectWorkspace:  projectConfig.Workspace,
			Terragrunt:        projectConfig.Terragrunt,
			OpenTofu:          projectConfig.OpenTofu,
			Commands:          []string{command},
			ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
			PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
			PullRequestNumber: nil,
			EventName:         "manual_invocation",
			RequestedBy:       actor,
			Namespace:         repository,
			StateEnvVars:      stateEnvVars,
			CommandEnvVars:    commandEnvVars,
		}
		err := digger.RunJob(jobs, repository, actor, &bitbucketService, policyChecker, planStorage, backendApi, nil, currentDir)
		if err != nil {
			usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to run commands. %s", err), 8)
		}
	} else if runningMode == "drift-detection" {

		for _, projectConfig := range diggerConfig.Projects {
			if !projectConfig.DriftDetection {
				continue
			}
			workflow := diggerConfig.Workflows[projectConfig.Workflow]

			stateEnvVars, commandEnvVars := digger_config.CollectTerraformEnvConfig(workflow.EnvVars)

			StateEnvProvider, CommandEnvProvider := orchestrator.GetStateAndCommandProviders(projectConfig)

			job := orchestrator.Job{
				ProjectName:        projectConfig.Name,
				ProjectDir:         projectConfig.Dir,
				ProjectWorkspace:   projectConfig.Workspace,
				Terragrunt:         projectConfig.Terragrunt,
				OpenTofu:           projectConfig.OpenTofu,
				Commands:           []string{"digger drift-detect"},
				ApplyStage:         orchestrator.ToConfigStage(workflow.Apply),
				PlanStage:          orchestrator.ToConfigStage(workflow.Plan),
				CommandEnvVars:     commandEnvVars,
				StateEnvVars:       stateEnvVars,
				RequestedBy:        actor,
				Namespace:          repository,
				EventName:          "drift-detect",
				CommandEnvProvider: CommandEnvProvider,
				StateEnvProvider:   StateEnvProvider,
			}
			err := digger.RunJob(job, repository, actor, &bitbucketService, policyChecker, nil, backendApi, nil, currentDir)
			if err != nil {
				usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to run commands. %s", err), 8)
			}
		}
	} else {
		var jobs []orchestrator.Job
		if os.Getenv("BITBUCKET_PR_ID") == "" && os.Getenv("BITBUCKET_BRANCH") == os.Getenv("DEFAULT_BRANCH") {
			for _, projectConfig := range diggerConfig.Projects {

				workflow := diggerConfig.Workflows[projectConfig.Workflow]
				log.Printf("workflow: %v", workflow)

				stateEnvVars, commandEnvVars := digger_config.CollectTerraformEnvConfig(workflow.EnvVars)

				job := orchestrator.Job{
					ProjectName:      projectConfig.Name,
					ProjectDir:       projectConfig.Dir,
					ProjectWorkspace: projectConfig.Workspace,
					Terragrunt:       projectConfig.Terragrunt,
					OpenTofu:         projectConfig.OpenTofu,
					Commands:         workflow.Configuration.OnCommitToDefault,
					ApplyStage:       orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:        orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:   commandEnvVars,
					StateEnvVars:     stateEnvVars,
					RequestedBy:      actor,
					Namespace:        repository,
					EventName:        "commit_to_default",
				}
				err := digger.RunJob(job, repository, actor, &bitbucketService, policyChecker, nil, backendApi, nil, currentDir)
				if err != nil {
					usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to run commands. %s", err), 8)
				}
			}
		} else if os.Getenv("BITBUCKET_PR_ID") == "" {
			for _, projectConfig := range diggerConfig.Projects {

				workflow := diggerConfig.Workflows[projectConfig.Workflow]

				stateEnvVars, commandEnvVars := digger_config.CollectTerraformEnvConfig(workflow.EnvVars)

				job := orchestrator.Job{
					ProjectName:      projectConfig.Name,
					ProjectDir:       projectConfig.Dir,
					ProjectWorkspace: projectConfig.Workspace,
					Terragrunt:       projectConfig.Terragrunt,
					OpenTofu:         projectConfig.OpenTofu,
					Commands:         []string{"digger plan"},
					ApplyStage:       orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:        orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:   commandEnvVars,
					StateEnvVars:     stateEnvVars,
					RequestedBy:      actor,
					Namespace:        repository,
					EventName:        "commit_to_default",
				}
				err := digger.RunJob(job, repository, actor, &bitbucketService, policyChecker, nil, backendApi, nil, currentDir)
				if err != nil {
					usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to run commands. %s", err), 8)
				}
			}
		} else if os.Getenv("BITBUCKET_PR_ID") != "" {
			prNumber, err := strconv.Atoi(os.Getenv("BITBUCKET_PR_ID"))
			if err != nil {
				usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to parse PR number. %s", err), 4)
			}
			impactedProjects, err := bitbucket.FindImpactedProjectsInBitbucket(diggerConfig, prNumber, &bitbucketService)

			if err != nil {
				usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to find impacted projects. %s", err), 5)
			}
			if len(impactedProjects) == 0 {
				usage.ReportErrorAndExit(actor, "No projects impacted", 0)
			}

			impactedProjectsMsg := getImpactedProjectsAsString(impactedProjects, prNumber)
			log.Println(impactedProjectsMsg)
			if err != nil {
				usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to find impacted projects. %s", err), 5)
			}

			for _, project := range impactedProjects {
				workflow := diggerConfig.Workflows[project.Workflow]

				stateEnvVars, commandEnvVars := digger_config.CollectTerraformEnvConfig(workflow.EnvVars)

				job := orchestrator.Job{
					ProjectName:       project.Name,
					ProjectDir:        project.Dir,
					ProjectWorkspace:  project.Workspace,
					Terragrunt:        project.Terragrunt,
					OpenTofu:          project.OpenTofu,
					Commands:          workflow.Configuration.OnPullRequestPushed,
					ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:    commandEnvVars,
					StateEnvVars:      stateEnvVars,
					PullRequestNumber: &prNumber,
					RequestedBy:       actor,
					Namespace:         repository,
					EventName:         "pull_request",
				}
				jobs = append(jobs, job)
			}

			reporter := reporting.CiReporter{
				CiService:      &bitbucketService,
				PrNumber:       prNumber,
				ReportStrategy: reportingStrategy,
			}

			log.Println("Bitbucket trigger converted to commands successfully")

			logCommands(jobs)

			planStorage := storage.NewPlanStorage("", repoOwner, repositoryName, actor, nil)

			jobs = digger.SortedCommandsByDependency(jobs, &dependencyGraph)

			_, _, err = digger.RunJobs(jobs, &bitbucketService, &bitbucketService, lock, &reporter, planStorage, policyChecker, comment_updater.NoopCommentUpdater{}, backendApi, "", false, false, 0, currentDir)
			if err != nil {
				usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to run commands. %s", err), 8)
			}
		} else {
			usage.ReportErrorAndExit(actor, "Failed to detect running mode", 1)
		}

	}

	usage.ReportErrorAndExit(actor, "Digger finished successfully", 0)
}

func exec(actor string, projectName string, repoNamespace string, command string, prNumber int, lock core_locking.Lock, policyChecker core_policy.Checker, prService orchestrator.PullRequestService, orgService orchestrator.OrgService, reporter reporting.Reporter, backendApi core_backend.Api) {

	//SCMOrganisation, SCMrepository := utils.ParseRepoNamespace(runConfig.RepoNamespace)
	currentDir, err := os.Getwd()
	if err != nil {

		usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to get current dir. %s", err), 4)

	}

	planStorage := storage.NewPlanStorage("", "", "", actor, nil)

	diggerConfig, _, dependencyGraph, err := digger_config.LoadDiggerConfig("./", true)
	if err != nil {
		usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to load digger config. %s", err), 4)
	}
	//impactedProjects := diggerConfig.GetModifiedProjects(strings.Split(runConfig.FilesChanged, ","))
	impactedProjects := diggerConfig.GetProjects(projectName)
	jobs, _, err := orchestrator.ConvertProjectsToJobs(actor, repoNamespace, command, prNumber, impactedProjects, nil, diggerConfig.Workflows)
	if err != nil {
		usage.ReportErrorAndExit(actor, fmt.Sprintf("Failed to convert impacted projects to commands. %s", err), 4)
	}

	jobs = digger.SortedCommandsByDependency(jobs, &dependencyGraph)
	_, _, err = digger.RunJobs(jobs, prService, orgService, lock, reporter, planStorage, policyChecker, comment_updater.NoopCommentUpdater{}, backendApi, "", false, false, 123, currentDir)
}

/*
Exit codes:
0 - No errors
1 - Failed to read digger digger_config
2 - Failed to create lock provider
3 - Failed to find auth token
4 - Failed to initialise CI context
5 -
6 - failed to process CI event
7 - failed to convert event to command
8 - failed to execute command
10 - No CI detected
*/

func main() {
	if len(os.Args) == 1 {
		os.Args = append([]string{os.Args[0]}, "default")
	}
	if err := rootCmd.Execute(); err != nil {
		usage.ReportErrorAndExit("", fmt.Sprintf("Error occured during command exec: %v", err), 8)
	}

}

func getImpactedProjectsAsString(projects []digger_config.Project, prNumber int) string {
	msg := fmt.Sprintf("Following projects are impacted by pull request #%d\n", prNumber)
	for _, p := range projects {
		msg += fmt.Sprintf("- %s\n", p.Name)
	}
	return msg
}

func logCommands(projectCommands []orchestrator.Job) {
	logMessage := fmt.Sprintf("Following commands are going to be executed:\n")
	for _, pc := range projectCommands {
		logMessage += fmt.Sprintf("project: %s: commands: ", pc.ProjectName)
		for _, c := range pc.Commands {
			logMessage += fmt.Sprintf("\"%s\", ", c)
		}
		logMessage += "\n"
	}
	log.Print(logMessage)
}

func init() {
	log.SetOutput(os.Stdout)

	if os.Getenv("DEBUG") == "true" {
		log.SetFlags(log.Ltime | log.Lshortfile)
	}
}
