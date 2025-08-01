package swarm

// RuntimeType is the type of runtime used for the TaskSpec
type RuntimeType string

// RuntimeURL is the proto type url
type RuntimeURL string

const (
	// RuntimeContainer is the container based runtime
	RuntimeContainer RuntimeType = "container"
	// RuntimePlugin is the plugin based runtime
	RuntimePlugin RuntimeType = "plugin"
	// RuntimeNetworkAttachment is the network attachment runtime
	RuntimeNetworkAttachment RuntimeType = "attachment"

	// RuntimeURLContainer is the proto url for the container type
	RuntimeURLContainer RuntimeURL = "types.docker.com/RuntimeContainer"
	// RuntimeURLPlugin is the proto url for the plugin type
	RuntimeURLPlugin RuntimeURL = "types.docker.com/RuntimePlugin"
)

// NetworkAttachmentSpec represents the runtime spec type for network
// attachment tasks
type NetworkAttachmentSpec struct {
	ContainerID string
}

// RuntimeSpec defines the base payload which clients can specify for creating
// a service with the plugin runtime.
type RuntimeSpec struct {
	Name       string              `json:"name,omitempty"`
	Remote     string              `json:"remote,omitempty"`
	Privileges []*RuntimePrivilege `json:"privileges,omitempty"`
	Disabled   bool                `json:"disabled,omitempty"`
	Env        []string            `json:"env,omitempty"`
}

// RuntimePrivilege describes a permission the user has to accept
// upon installing a plugin.
type RuntimePrivilege struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Value       []string `json:"value,omitempty"`
}
