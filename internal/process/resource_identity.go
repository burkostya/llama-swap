package process

// ResourceIdentity is persisted by the resource coordinator. PID reuse and
// machine reboot must never turn recovery into signalling an unrelated group.
type ResourceIdentity struct {
	PID       int    `json:"pid"`
	PGID      int    `json:"pgid"`
	StartTime string `json:"startTime"`
	BootID    string `json:"bootID"`
}

type ResourceProcess interface {
	Process
	ResourceIdentity() *ResourceIdentity
	ResourceStopError() string
	ForceResourceStop() error
}

func (p *ProcessCommand) ResourceIdentity() *ResourceIdentity {
	identity := p.resourceIdentity.Load()
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

func (p *ProcessCommand) ResourceStopError() string {
	if err := p.resourceStopError.Load(); err != nil {
		return *err
	}
	return ""
}

func (p *ProcessCommand) ForceResourceStop() error {
	p.resourceForced.Store(true)
	identity := p.ResourceIdentity()
	if identity == nil {
		return nil
	}
	return KillResourceGroup(*identity)
}
