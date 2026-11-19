package perms

import "guarde.sh/nala/pkg/bitperms"

var Org = bitperms.MustSet(
	"jobs.view",
	"jobs.enqueue",
	"jobs.cancel",
	"jobs.retry",
	"jobs.delete",
	"logs.view",
	"queues.manage",
	"members.view",
	"members.manage",
	"keys.manage",
	"org.manage",
	"audit.view",

	"jobs.network",
	"images.manage",
	"hosts.manage",
	"invites.manage",
	"workflows.view",
	"workflows.manage",
	"workflows.run",
	"settings.manage",
)

var (
	JobsView        = Org.Must("jobs.view")
	JobsEnqueue     = Org.Must("jobs.enqueue")
	JobsCancel      = Org.Must("jobs.cancel")
	JobsRetry       = Org.Must("jobs.retry")
	JobsDelete      = Org.Must("jobs.delete")
	LogsView        = Org.Must("logs.view")
	QueuesManage    = Org.Must("queues.manage")
	MembersView     = Org.Must("members.view")
	MembersManage   = Org.Must("members.manage")
	KeysManage      = Org.Must("keys.manage")
	OrgManage       = Org.Must("org.manage")
	AuditView       = Org.Must("audit.view")
	JobsNetwork     = Org.Must("jobs.network")
	ImagesManage    = Org.Must("images.manage")
	HostsManage     = Org.Must("hosts.manage")
	InvitesManage   = Org.Must("invites.manage")
	WorkflowsView   = Org.Must("workflows.view")
	WorkflowsManage = Org.Must("workflows.manage")
	WorkflowsRun    = Org.Must("workflows.run")
	SettingsManage  = Org.Must("settings.manage")
)

var Descriptions = map[string]string{
	"jobs.view":        "See jobs, queues and their status",
	"jobs.enqueue":     "Submit new jobs",
	"jobs.cancel":      "Cancel pending and running jobs",
	"jobs.retry":       "Retry dead and cancelled jobs",
	"jobs.delete":      "Delete jobs and their logs",
	"logs.view":        "Read job output",
	"queues.manage":    "Pause and resume queues",
	"members.view":     "See who is in the organisation",
	"members.manage":   "Change members' permissions and remove members",
	"keys.manage":      "Create and revoke API keys",
	"org.manage":       "Rename and delete the organisation",
	"audit.view":       "Read the audit log",
	"jobs.network":     "Submit jobs with internet access",
	"images.manage":    "Choose which images jobs may run",
	"hosts.manage":     "Choose which internet hosts jobs may reach",
	"invites.manage":   "Invite people and revoke invites",
	"workflows.view":   "See workflows and their runs",
	"workflows.manage": "Create, edit and delete workflows",
	"workflows.run":    "Start and cancel workflow runs",
	"settings.manage":  "Change limits and turn internet access on or off",
}

var (
	RoleViewer    = JobsView | LogsView | MembersView | WorkflowsView
	RoleDeveloper = RoleViewer | JobsEnqueue | JobsCancel | JobsRetry | WorkflowsManage | WorkflowsRun
	RoleOperator  = RoleDeveloper | JobsDelete | QueuesManage
	RoleOwner     = Org.All()
)

var Roles = map[string]bitperms.Perm{
	"viewer":    RoleViewer,
	"developer": RoleDeveloper,
	"operator":  RoleOperator,
	"owner":     RoleOwner,
}

var RoleOrder = []string{"viewer", "developer", "operator", "owner"}

var KeyAllowed = JobsView | JobsEnqueue | JobsCancel | JobsRetry | JobsDelete | LogsView | QueuesManage |
	JobsNetwork | WorkflowsView | WorkflowsRun

func RoleName(p bitperms.Perm) string {
	for i := len(RoleOrder) - 1; i >= 0; i-- {
		if Roles[RoleOrder[i]] == p {
			return RoleOrder[i]
		}
	}
	return "custom"
}

var Platform = bitperms.MustSet(
	"platform.read",
	"platform.users.manage",
	"platform.catalogue.manage",
)

var (
	PlatformRead            = Platform.Must("platform.read")
	PlatformUsersManage     = Platform.Must("platform.users.manage")
	PlatformCatalogueManage = Platform.Must("platform.catalogue.manage")
	PlatformAdmin           = Platform.All()
)
