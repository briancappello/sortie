package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// Environment kinds accepted by environment.kind.
const (
	// EnvironmentKindLocal runs workspace code on the host, in the host
	// workspace directory. It is the kind when environment.kind is
	// absent.
	EnvironmentKindLocal = "local"

	// EnvironmentKindSmolvm runs workspace code in one smolvm microVM
	// per issue workspace.
	EnvironmentKindSmolvm = "smolvm"
)

// Defaults applied to the environment.smolvm block.
const (
	defaultSmolvmBinary     = "smolvm"
	defaultRepoBranchPrefix = "sortie/"
	defaultRepoMaxBundleMiB = 512
)

// Field paths of the environment block, and the host name length limit.
const (
	environmentField          = "environment"
	environmentHostPrefixPath = environmentField + ".host_prefix"
	smolvmField               = environmentField + ".smolvm"
	smolvmRepositoryField     = smolvmField + ".repository"
	smolvmRepositoriesField   = smolvmField + ".repositories"
	smolvmDefaultRepoField    = smolvmField + ".default_repository"
	smolvmCredentialsField    = smolvmField + ".credentials"
	maxHostNameLength         = 253
)

var (
	envVarNamePattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repositoryNamePattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	credentialNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	hostLabelPattern         = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	environmentKindsAccepted = []string{EnvironmentKindLocal, EnvironmentKindSmolvm}
)

// EnvironmentConfig holds the parsed environment block: where the
// workspace code of every issue runs.
type EnvironmentConfig struct {
	// Kind is the environment kind, [EnvironmentKindLocal] when the
	// block or its kind key is absent.
	Kind string

	// HostPrefix is the argv that stays on the host in front of the
	// environment's launch command for an agent session and its
	// credential check. Nil when absent; never an empty list.
	HostPrefix []string

	// Smolvm holds the smolvm settings. It is parsed and validated only
	// when Kind is [EnvironmentKindSmolvm]; under any other kind the
	// smolvm block is accepted and ignored, and this field is the zero
	// value.
	Smolvm SmolvmConfig
}

// SmolvmConfig holds the environment.smolvm settings.
type SmolvmConfig struct {
	// Binary is the smolvm program, "smolvm" by default, with ~ and
	// $VAR expanded.
	Binary string

	// Pack is the path of the pack file every VM is created from, with
	// ~ and $VAR expanded. Validation guarantees it named a readable
	// regular file when the configuration was built.
	Pack string

	// CPUs, MemoryMiB and StorageGiB size each VM. 0 selects smolvm's
	// own default.
	CPUs       int
	MemoryMiB  int
	StorageGiB int

	// AllowHosts lists the hosts a VM may reach, each an exact host name
	// or a "*.domain" pattern. Empty means no egress except the hosts of
	// the credential bindings.
	AllowHosts []string

	// Credentials lists the credential bindings, in written order.
	Credentials []SmolvmCredential

	// Repository configures the host-owned repository the guest
	// workspace is seeded from and exported to by default: the single
	// repository block, or the default_repository entry of the
	// repositories list. Nil when neither is set.
	Repository *SmolvmRepository

	// Repositories lists every repository Sortie may open, in written
	// order: the repositories list, or the single repository block as
	// its one entry. Nil when neither is set.
	Repositories []SmolvmRepository

	// DefaultRepository names the entry of Repositories an issue that
	// names no repository uses. Empty for the single repository block.
	DefaultRepository string

	// PassEnv lists host variable names copied into the guest in plain
	// text.
	PassEnv []string

	// SSHAgent forwards the host SSH agent into the guest.
	SSHAgent bool
}

// SmolvmCredential binds a host environment variable to the guest
// variable of the same name for a set of hosts. The guest sees only a
// placeholder; the real value is substituted on HTTPS requests to the
// listed hosts.
type SmolvmCredential struct {
	// Name is the smolvm credential name, matching [A-Za-z0-9_-]+.
	Name string

	// Env is the host variable name, which is also the guest variable
	// name.
	Env string

	// Hosts lists at least one host, each an exact host name or a
	// "*.domain" pattern.
	Hosts []string

	// Header optionally names the request header the value is set in.
	Header string
}

// SmolvmRepository configures the host-owned repository mirror.
type SmolvmRepository struct {
	// Name identifies the repository to plan stories and issues.
	// Required in the repositories list, matching
	// [a-z0-9]+(-[a-z0-9]+)*; optional in the single repository block.
	Name string

	// URL is the upstream the host fetches and pushes with its own
	// credentials. Required.
	URL string

	// Base is the upstream branch a new workspace starts from.
	// Required.
	Base string

	// BranchPrefix precedes the workspace key in the issue branch name.
	// "sortie/" by default.
	BranchPrefix string

	// Provider optionally names the SCM adapter that opens pull
	// requests.
	Provider string

	// Project names the repository for Provider. Required when Provider
	// is set.
	Project string

	// MaxBundleMiB caps the size of a bundle exported from the guest.
	// Positive; 512 by default.
	MaxBundleMiB int
}

// buildEnvironmentConfig parses the top-level environment block. The
// worker extension section is consulted for the rule that the smolvm
// kind excludes SSH workers. Returns the first [*ConfigError] found.
func buildEnvironmentConfig(raw map[string]any, worker map[string]any) (EnvironmentConfig, error) {
	cfg := EnvironmentConfig{Kind: EnvironmentKindLocal}

	value, ok := raw[environmentField]
	if !ok || value == nil {
		return cfg, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return EnvironmentConfig{}, typeError(environmentField, "mapping", value)
	}

	if rawKind, exists := m["kind"]; exists && rawKind != nil {
		kind, isString := rawKind.(string)
		if !isString {
			return EnvironmentConfig{}, typeError(environmentField+".kind", "string", rawKind)
		}
		if kind != EnvironmentKindLocal && kind != EnvironmentKindSmolvm {
			return EnvironmentConfig{}, &ConfigError{
				Field:   environmentField + ".kind",
				Message: fmt.Sprintf("unknown environment kind %q; valid: %s", kind, strings.Join(environmentKindsAccepted, ", ")),
			}
		}
		cfg.Kind = kind
	}

	prefix, err := parseHostPrefix(m)
	if err != nil {
		return EnvironmentConfig{}, err
	}
	cfg.HostPrefix = prefix

	if cfg.Kind != EnvironmentKindSmolvm {
		return cfg, nil
	}

	if hosts := WorkerSSHHosts(worker); len(hosts) > 0 {
		return EnvironmentConfig{}, &ConfigError{
			Field:   environmentField + ".kind",
			Message: fmt.Sprintf("environment.kind smolvm cannot be combined with worker.ssh_hosts (%d host(s) listed); remove one of the two settings", len(hosts)),
		}
	}

	smolvm, err := buildSmolvmConfig(m["smolvm"])
	if err != nil {
		return EnvironmentConfig{}, err
	}
	cfg.Smolvm = smolvm
	return cfg, nil
}

// parseHostPrefix reads environment.host_prefix: absent or null yields
// nil; otherwise a non-empty list of non-empty strings.
func parseHostPrefix(m map[string]any) ([]string, error) {
	raw, exists := m["host_prefix"]
	if !exists || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, typeError(environmentHostPrefixPath, "list", raw)
	}
	if len(list) == 0 {
		return nil, &ConfigError{
			Field:   environmentHostPrefixPath,
			Message: "must not be an empty list; remove the key to launch without a host prefix",
		}
	}
	prefix := make([]string, 0, len(list))
	for i, elem := range list {
		field := fmt.Sprintf("%s[%d]", environmentHostPrefixPath, i)
		s, isString := elem.(string)
		if !isString {
			return nil, typeError(field, "string", elem)
		}
		if s == "" {
			return nil, &ConfigError{Field: field, Message: "must not be an empty string"}
		}
		prefix = append(prefix, s)
	}
	return prefix, nil
}

// buildSmolvmConfig parses environment.smolvm for the smolvm kind.
func buildSmolvmConfig(value any) (SmolvmConfig, error) {
	var m map[string]any
	if value != nil {
		var ok bool
		m, ok = value.(map[string]any)
		if !ok {
			return SmolvmConfig{}, typeError(smolvmField, "mapping", value)
		}
	}

	binary, err := smolvmPathField(m, "binary")
	if err != nil {
		return SmolvmConfig{}, err
	}
	if binary == "" {
		binary = defaultSmolvmBinary
	}

	pack, err := smolvmPathField(m, "pack")
	if err != nil {
		return SmolvmConfig{}, err
	}
	if err := checkPackFile(pack); err != nil {
		return SmolvmConfig{}, err
	}

	cfg := SmolvmConfig{Binary: binary, Pack: pack}

	for _, sizing := range []struct {
		key string
		dst *int
	}{
		{"cpus", &cfg.CPUs},
		{"memory_mib", &cfg.MemoryMiB},
		{"storage_gib", &cfg.StorageGiB},
	} {
		n, err := coerceIntField(m, sizing.key, smolvmField+"."+sizing.key)
		if err != nil {
			return SmolvmConfig{}, err
		}
		if n < 0 {
			return SmolvmConfig{}, &ConfigError{
				Field:   smolvmField + "." + sizing.key,
				Message: fmt.Sprintf("must not be negative, got %d", n),
			}
		}
		*sizing.dst = n
	}

	cfg.AllowHosts, err = parseHostList(m, "allow_hosts", smolvmField+".allow_hosts", false)
	if err != nil {
		return SmolvmConfig{}, err
	}

	cfg.Credentials, err = parseSmolvmCredentials(m)
	if err != nil {
		return SmolvmConfig{}, err
	}

	if err := parseSmolvmRepositories(m, &cfg); err != nil {
		return SmolvmConfig{}, err
	}

	cfg.PassEnv, err = parsePassEnv(m)
	if err != nil {
		return SmolvmConfig{}, err
	}

	if rawAgent, exists := m["ssh_agent"]; exists && rawAgent != nil {
		b, isBool := rawAgent.(bool)
		if !isBool {
			return SmolvmConfig{}, typeError(smolvmField+".ssh_agent", "boolean", rawAgent)
		}
		cfg.SSHAgent = b
	}

	return cfg, nil
}

// smolvmPathField reads a string path key of environment.smolvm and
// expands ~ and $VAR in it, as workspace.root is expanded.
func smolvmPathField(m map[string]any, key string) (string, error) {
	field := smolvmField + "." + key
	s, present, err := requireStringField(m, key, field)
	if err != nil || !present {
		return "", err
	}
	expanded, err := ExpandPath(s, true)
	if err != nil {
		return "", &ConfigError{Field: field, Message: err.Error()}
	}
	return expanded, nil
}

// checkPackFile requires the pack path to be set and to name a
// readable regular file.
func checkPackFile(pack string) error {
	field := smolvmField + ".pack"
	if pack == "" {
		return &ConfigError{Field: field, Message: "is required with environment.kind smolvm"}
	}
	info, err := os.Stat(pack)
	if err != nil {
		return &ConfigError{Field: field, Message: fmt.Sprintf("pack %q is not readable: %v", pack, err)}
	}
	if !info.Mode().IsRegular() {
		return &ConfigError{Field: field, Message: fmt.Sprintf("pack %q is not a regular file", pack)}
	}
	f, err := os.Open(pack) //nolint:gosec // G304: the operator names the pack; it is opened only to prove it is readable.
	if err != nil {
		return &ConfigError{Field: field, Message: fmt.Sprintf("pack %q is not readable: %v", pack, err)}
	}
	_ = f.Close()
	return nil
}

// parseHostList reads a list of host patterns at key. When required
// holds, an absent or empty list is an error.
func parseHostList(m map[string]any, key, field string, required bool) ([]string, error) {
	raw, exists := m[key]
	if !exists || raw == nil {
		if required {
			return nil, &ConfigError{Field: field, Message: "is required and must list at least one host"}
		}
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, typeError(field, "list", raw)
	}
	if required && len(list) == 0 {
		return nil, &ConfigError{Field: field, Message: "must list at least one host"}
	}
	hosts := make([]string, 0, len(list))
	for i, elem := range list {
		elemField := fmt.Sprintf("%s[%d]", field, i)
		s, isString := elem.(string)
		if !isString {
			return nil, typeError(elemField, "string", elem)
		}
		if err := validateHostPattern(s); err != nil {
			return nil, &ConfigError{Field: elemField, Message: fmt.Sprintf("invalid host %q: %v", s, err)}
		}
		hosts = append(hosts, s)
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	return hosts, nil
}

// validateHostPattern accepts an exact host name or "*.domain", where
// domain is itself a host name. Schemes, ports, paths and wildcards in
// any other position are rejected.
func validateHostPattern(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	name := s
	if rest, found := strings.CutPrefix(s, "*."); found {
		name = rest
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("a wildcard is allowed only as a leading \"*.\"")
	}
	if len(name) > maxHostNameLength {
		return fmt.Errorf("longer than %d characters", maxHostNameLength)
	}
	for label := range strings.SplitSeq(name, ".") {
		if !hostLabelPattern.MatchString(label) {
			return fmt.Errorf("want an exact host name or \"*.domain\"")
		}
	}
	return nil
}

// parseSmolvmCredentials reads environment.smolvm.credentials.
func parseSmolvmCredentials(m map[string]any) ([]SmolvmCredential, error) {
	raw, exists := m["credentials"]
	if !exists || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, typeError(smolvmCredentialsField, "list", raw)
	}
	creds := make([]SmolvmCredential, 0, len(list))
	seen := make(map[string]int, len(list))
	for i, elem := range list {
		field := fmt.Sprintf("%s[%d]", smolvmCredentialsField, i)
		entry, isMap := elem.(map[string]any)
		if !isMap {
			return nil, typeError(field, "mapping", elem)
		}

		name, err := requiredString(entry, "name", field+".name")
		if err != nil {
			return nil, err
		}
		if !credentialNamePattern.MatchString(name) {
			return nil, &ConfigError{
				Field:   field + ".name",
				Message: fmt.Sprintf("invalid credential name %q; use letters, digits, '_' and '-'", name),
			}
		}
		if first, dup := seen[name]; dup {
			return nil, &ConfigError{
				Field:   field + ".name",
				Message: fmt.Sprintf("credential name %q is already used by %s[%d]", name, smolvmCredentialsField, first),
			}
		}
		seen[name] = i

		env, err := requiredString(entry, "env", field+".env")
		if err != nil {
			return nil, err
		}
		if !envVarNamePattern.MatchString(env) {
			return nil, &ConfigError{
				Field:   field + ".env",
				Message: fmt.Sprintf("invalid environment variable name %q", env),
			}
		}

		hosts, err := parseHostList(entry, "hosts", field+".hosts", true)
		if err != nil {
			return nil, err
		}

		header, present, err := requireStringField(entry, "header", field+".header")
		if err != nil {
			return nil, err
		}
		if present && header == "" {
			return nil, &ConfigError{Field: field + ".header", Message: "must not be an empty string; remove the key for the default"}
		}

		creds = append(creds, SmolvmCredential{Name: name, Env: env, Hosts: hosts, Header: header})
	}
	if len(creds) == 0 {
		return nil, nil
	}
	return creds, nil
}

// parsePassEnv reads environment.smolvm.pass_env, a list of host
// variable names.
func parsePassEnv(m map[string]any) ([]string, error) {
	field := smolvmField + ".pass_env"
	raw, exists := m["pass_env"]
	if !exists || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, typeError(field, "list", raw)
	}
	names := make([]string, 0, len(list))
	for i, elem := range list {
		elemField := fmt.Sprintf("%s[%d]", field, i)
		s, isString := elem.(string)
		if !isString {
			return nil, typeError(elemField, "string", elem)
		}
		if !envVarNamePattern.MatchString(s) {
			return nil, &ConfigError{Field: elemField, Message: fmt.Sprintf("invalid environment variable name %q", s)}
		}
		names = append(names, s)
	}
	if len(names) == 0 {
		return nil, nil
	}
	return names, nil
}

// parseSmolvmRepository reads environment.smolvm.repository, applying
// its defaults.
func parseSmolvmRepository(m map[string]any) (*SmolvmRepository, error) {
	raw, exists := m["repository"]
	if !exists || raw == nil {
		return nil, nil
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		return nil, typeError(smolvmRepositoryField, "mapping", raw)
	}
	return parseRepositoryEntry(entry, smolvmRepositoryField)
}

// RepositoryNamed returns the configured repository called name, and
// whether there is one. The single repository block answers its own
// name, which may be empty.
func (c SmolvmConfig) RepositoryNamed(name string) (*SmolvmRepository, bool) {
	for _, r := range c.AllRepositories() {
		if r.Name == name {
			return &r, true
		}
	}
	return nil, false
}

// AllRepositories returns Repositories, or the one Repository when only
// that is set.
func (c SmolvmConfig) AllRepositories() []SmolvmRepository {
	if len(c.Repositories) == 0 && c.Repository != nil {
		return []SmolvmRepository{*c.Repository}
	}
	return c.Repositories
}

// RepositoryNames lists the names of the configured repositories in
// written order, leaving out an unnamed single repository block.
func (c SmolvmConfig) RepositoryNames() []string {
	var names []string
	for _, r := range c.AllRepositories() {
		if r.Name != "" {
			names = append(names, r.Name)
		}
	}
	return names
}

// parseSmolvmRepositories reads environment.smolvm.repository, or
// environment.smolvm.repositories with default_repository, into cfg.
func parseSmolvmRepositories(m map[string]any, cfg *SmolvmConfig) error {
	single, err := parseSmolvmRepository(m)
	if err != nil {
		return err
	}
	rawList, listSet := m["repositories"]
	listSet = listSet && rawList != nil
	def, defSet, err := requireStringField(m, "default_repository", smolvmDefaultRepoField)
	if err != nil {
		return err
	}
	if single != nil {
		if listSet {
			return &ConfigError{Field: smolvmRepositoriesField, Message: "cannot be set together with repository; list the one repository under repositories instead"}
		}
		if defSet {
			return &ConfigError{Field: smolvmDefaultRepoField, Message: "applies to repositories, not to the single repository block"}
		}
		cfg.Repository = single
		cfg.Repositories = []SmolvmRepository{*single}
		return nil
	}
	if !listSet {
		if defSet {
			return &ConfigError{Field: smolvmDefaultRepoField, Message: "is set but repositories lists none"}
		}
		return nil
	}
	list, ok := rawList.([]any)
	if !ok {
		return typeError(smolvmRepositoriesField, "sequence", rawList)
	}
	if len(list) == 0 {
		return &ConfigError{Field: smolvmRepositoriesField, Message: "must list at least one repository"}
	}
	seen := map[string]bool{}
	repos := make([]SmolvmRepository, 0, len(list))
	for i, elem := range list {
		field := fmt.Sprintf("%s[%d]", smolvmRepositoriesField, i)
		entry, ok := elem.(map[string]any)
		if !ok {
			return typeError(field, "mapping", elem)
		}
		repo, err := parseRepositoryEntry(entry, field)
		if err != nil {
			return err
		}
		if repo.Name == "" {
			return &ConfigError{Field: field + ".name", Message: "is required and must be a non-empty string"}
		}
		if seen[repo.Name] {
			return &ConfigError{Field: field + ".name", Message: fmt.Sprintf("%q is listed more than once", repo.Name)}
		}
		seen[repo.Name] = true
		repos = append(repos, *repo)
	}
	if def == "" {
		if len(repos) > 1 {
			return &ConfigError{Field: smolvmDefaultRepoField, Message: "is required when repositories lists more than one repository"}
		}
		def = repos[0].Name
	}
	if !seen[def] {
		return &ConfigError{Field: smolvmDefaultRepoField, Message: fmt.Sprintf("names %q, which repositories does not list", def)}
	}
	cfg.Repositories = repos
	cfg.DefaultRepository = def
	cfg.Repository, _ = cfg.RepositoryNamed(def)
	return nil
}

// parseRepositoryEntry reads one repository mapping at field, applying
// its defaults.
func parseRepositoryEntry(entry map[string]any, field string) (*SmolvmRepository, error) {
	name, _, err := requireStringField(entry, "name", field+".name")
	if err != nil {
		return nil, err
	}
	if name != "" && !repositoryNamePattern.MatchString(name) {
		return nil, &ConfigError{Field: field + ".name", Message: fmt.Sprintf("%q must match [a-z0-9]+(-[a-z0-9]+)*", name)}
	}
	smolvmRepositoryField := field

	url, err := requiredString(entry, "url", smolvmRepositoryField+".url")
	if err != nil {
		return nil, err
	}
	base, err := requiredString(entry, "base", smolvmRepositoryField+".base")
	if err != nil {
		return nil, err
	}

	prefix, present, err := requireStringField(entry, "branch_prefix", smolvmRepositoryField+".branch_prefix")
	if err != nil {
		return nil, err
	}
	if !present {
		prefix = defaultRepoBranchPrefix
	}

	provider, _, err := requireStringField(entry, "provider", smolvmRepositoryField+".provider")
	if err != nil {
		return nil, err
	}
	project, _, err := requireStringField(entry, "project", smolvmRepositoryField+".project")
	if err != nil {
		return nil, err
	}
	if provider != "" && project == "" {
		return nil, &ConfigError{
			Field:   smolvmRepositoryField + ".project",
			Message: fmt.Sprintf("is required when repository.provider is set (provider %q)", provider),
		}
	}

	maxBundle := defaultRepoMaxBundleMiB
	if v, set := entry["max_bundle_mib"]; set && v != nil {
		n, err := coerceIntField(entry, "max_bundle_mib", smolvmRepositoryField+".max_bundle_mib")
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, &ConfigError{
				Field:   smolvmRepositoryField + ".max_bundle_mib",
				Message: fmt.Sprintf("must be a positive integer, got %d", n),
			}
		}
		maxBundle = n
	}

	return &SmolvmRepository{
		Name:         name,
		URL:          url,
		Base:         base,
		BranchPrefix: prefix,
		Provider:     provider,
		Project:      project,
		MaxBundleMiB: maxBundle,
	}, nil
}

// requiredString reads a required non-empty string key.
func requiredString(m map[string]any, key, field string) (string, error) {
	s, present, err := requireStringField(m, key, field)
	if err != nil {
		return "", err
	}
	if !present || s == "" {
		return "", &ConfigError{Field: field, Message: "is required and must be a non-empty string"}
	}
	return s, nil
}

// typeError reports a value of the wrong YAML type at field.
func typeError(field, want string, got any) *ConfigError {
	return &ConfigError{
		Field:   field,
		Message: "expected " + want + ", got " + typeutil.DescribeYAMLType(got),
	}
}

// RepositoryLabelPrefix starts the label plan sync keeps on a story that
// names its repository: the prefix followed by the repository's name.
const RepositoryLabelPrefix = "sortie-repository:"

// IssueRepositoryName returns the repository name the labels of an issue
// carry under [RepositoryLabelPrefix], or "" when they carry none. Labels
// compare case-insensitively.
func IssueRepositoryName(labels []string) string {
	for _, l := range labels {
		if name, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(l)), RepositoryLabelPrefix); ok && name != "" {
			return name
		}
	}
	return ""
}
