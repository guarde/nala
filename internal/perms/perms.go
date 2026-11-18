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
)

var (
	JobsView      = Org.Must("jobs.view")
	JobsEnqueue   = Org.Must("jobs.enqueue")
	JobsCancel    = Org.Must("jobs.cancel")
	JobsRetry     = Org.Must("jobs.retry")
	JobsDelete    = Org.Must("jobs.delete")
	LogsView      = Org.Must("logs.view")
	QueuesManage  = Org.Must("queues.manage")
	MembersView   = Org.Must("members.view")
	MembersManage = Org.Must("members.manage")
	KeysManage    = Org.Must("keys.manage")
	OrgManage     = Org.Must("org.manage")
	AuditView     = Org.Must("audit.view")
)

var (
	RoleViewer    = JobsView | LogsView | MembersView
	RoleDeveloper = RoleViewer | JobsEnqueue | JobsCancel | JobsRetry
	RoleOperator  = RoleDeveloper | JobsDelete | QueuesManage
	RoleOwner     = Org.All()
)

var Roles = map[string]bitperms.Perm{
	"viewer":    RoleViewer,
	"developer": RoleDeveloper,
	"operator":  RoleOperator,
	"owner":     RoleOwner,
}

var KeyAllowed = JobsView | JobsEnqueue | JobsCancel | JobsRetry | JobsDelete | LogsView | QueuesManage

func RoleName(p bitperms.Perm) string {
	for _, n := range []string{"owner", "operator", "developer", "viewer"} {
		if Roles[n] == p {
			return n
		}
	}
	return "custom"
}

var Platform = bitperms.MustSet(
	"platform.read",
	"platform.users.manage",
)

var (
	PlatformRead        = Platform.Must("platform.read")
	PlatformUsersManage = Platform.Must("platform.users.manage")
	PlatformAdmin       = Platform.All()
)
