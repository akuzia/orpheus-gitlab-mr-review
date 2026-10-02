package gitlab

import "time"

type User struct {
	ID       int64
	Username string
}

type DiffRefs struct {
	BaseSHA  string
	StartSHA string
	HeadSHA  string
}

type MergeRequest struct {
	ID           int64
	ProjectID    int64
	IID          int64
	Title        string
	Description  string
	State        string
	SourceBranch string
	TargetBranch string
	WebURL       string
	Draft        bool
	UpdatedAt    time.Time
	Reviewers    []User
	DiffRefs     DiffRefs
}

type Project struct {
	ID                int64
	PathWithNamespace string
	HTTPURLToRepo     string
	SSHURLToRepo      string
	WebURL            string
	Archived          bool
}

type Position struct {
	BaseSHA      string
	StartSHA     string
	HeadSHA      string
	PositionType string
	NewPath      string
	NewLine      int64
	OldPath      string
	OldLine      int64
}

type DiffFile struct {
	OldPath     string
	NewPath     string
	Diff        string
	NewFile     bool
	RenamedFile bool
	DeletedFile bool
	Collapsed   bool
	TooLarge    bool
}

type ResolutionCause string

const (
	ResolutionCauseNone           ResolutionCause = ""
	ResolutionCauseExplicit       ResolutionCause = "explicit"
	ResolutionCauseOutdatedByPush ResolutionCause = "outdated_by_push"
	ResolutionCauseUnknown        ResolutionCause = "unknown"
)

type Note struct {
	ID             int64
	Body           string
	Author         User
	System         bool
	Resolvable     bool
	Resolved       bool
	ResolvedAt     *time.Time
	ResolvedBy     User
	ResolvedByPush bool
	Position       *Position
}

type Discussion struct {
	ID              string
	IndividualNote  bool
	Resolvable      bool
	Resolved        bool
	ResolutionCause ResolutionCause
	ResolvedAt      *time.Time
	ResolvedBy      User
	Notes           []Note
}

type ReviewInput struct {
	MergeRequest MergeRequest
	Project      Project
	Notes        []Note
}
