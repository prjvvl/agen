//! Agent bundle loading and validation.
//!
//! A bundle is an Agent Plugins 1.0 directory plus an `x-agen/` extension
//! directory (see docs/architecture.md §9). Loading validates every file
//! against the JSON Schemas in `spec/bundle/` and reports all problems at once.

use std::collections::BTreeMap;
use std::fmt;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::Value;
use sha2::{Digest, Sha256};

const PLUGIN_SCHEMA: &str = include_str!("../../../../spec/bundle/plugin.schema.json");
const MCP_SCHEMA: &str = include_str!("../../../../spec/bundle/mcp.schema.json");
const AGENT_SCHEMA: &str = include_str!("../../../../spec/bundle/agent.schema.json");
const HARNESS_SCHEMA: &str = include_str!("../../../../spec/bundle/harness.schema.json");
const CONFIG_SCHEMA: &str = include_str!("../../../../spec/bundle/config.schema.json");
const SECRETS_SCHEMA: &str = include_str!("../../../../spec/bundle/secrets.schema.json");

/// One problem found in a bundle.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Issue {
    /// Bundle-relative file, e.g. `x-agen/harness.json`.
    pub file: String,
    /// JSON pointer inside the file (empty for whole-file problems).
    pub path: String,
    pub message: String,
}

impl fmt::Display for Issue {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.path.is_empty() {
            write!(f, "{}: {}", self.file, self.message)
        } else {
            write!(f, "{} at {}: {}", self.file, self.path, self.message)
        }
    }
}

#[derive(Debug, thiserror::Error)]
#[error("invalid bundle {root}:\n{}", issues.iter().map(|i| format!("  - {i}")).collect::<Vec<_>>().join("\n"))]
pub struct BundleError {
    pub root: String,
    pub issues: Vec<Issue>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct AgentFront {
    pub name: String,
    pub description: String,
    #[serde(default)]
    pub skills: Vec<String>,
    #[serde(rename = "maxTurns")]
    pub max_turns: Option<u32>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Harness {
    pub provider: String,
    pub model: String,
    pub temperature: Option<f64>,
    pub max_output_tokens: Option<u32>,
    pub parallel_tool_calls: Option<bool>,
    pub base_url: Option<String>,
    pub api_key_secret: Option<String>,
    pub script: Option<String>,
    pub cassette: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum Action {
    Allow,
    #[default]
    Ask,
    Deny,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PermissionRule {
    pub tool: String,
    pub action: Action,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct Permissions {
    #[serde(default)]
    pub default: Action,
    #[serde(default)]
    pub rules: Vec<PermissionRule>,
    /// How long an "ask" waits for a decision ("30s", "10m", "1h").
    #[serde(default, rename = "approvalTimeout")]
    pub approval_timeout: Option<String>,
}

/// Parses "<n>ms|s|m|h".
pub fn parse_duration(s: &str) -> Option<std::time::Duration> {
    let s = s.trim();
    let (num, unit) = s.split_at(s.find(|c: char| !c.is_ascii_digit())?);
    let n: u64 = num.parse().ok()?;
    Some(match unit {
        "ms" => std::time::Duration::from_millis(n),
        "s" => std::time::Duration::from_secs(n),
        "m" => std::time::Duration::from_secs(n * 60),
        "h" => std::time::Duration::from_secs(n * 3600),
        _ => return None,
    })
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Budget {
    pub max_tokens_per_run: Option<u64>,
    pub max_usd_per_run: Option<f64>,
    pub max_usd_per_day: Option<f64>,
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Limits {
    pub max_delegation_depth: Option<u32>,
    pub max_fan_out: Option<u32>,
    pub max_total_delegations: Option<u32>,
}

/// Another deployment this agent may call (`call_agent`).
#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct Delegate {
    /// Deployment name.
    pub name: String,
    /// Namespace (default: the caller's).
    pub namespace: Option<String>,
    /// Fixed A2A endpoint (standalone use); otherwise resolved by the platform.
    pub url: Option<String>,
    /// Shown to the model.
    pub description: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct ToolMeta {
    pub side_effect: Option<bool>,
    pub idempotent: Option<bool>,
}

/// `x-agen/config.json`. Platform-only sections (scale, triggers, kind) are
/// kept as raw JSON; the platform owns their meaning.
#[derive(Debug, Clone, Deserialize, Default)]
pub struct Config {
    pub workspace: Option<String>,
    #[serde(default)]
    pub permissions: Permissions,
    #[serde(default)]
    pub budget: Budget,
    #[serde(default)]
    pub limits: Limits,
    #[serde(default)]
    pub tools: BTreeMap<String, ToolMeta>,
    #[serde(default)]
    pub delegates: Vec<Delegate>,
    pub kind: Option<String>,
    pub scale: Option<Value>,
    #[serde(default)]
    pub triggers: Vec<Value>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SecretRef {
    pub source: String,
    pub key: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct McpServer {
    pub command: Option<String>,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    pub url: Option<String>,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct McpConfig {
    #[serde(rename = "mcpServers")]
    pub servers: BTreeMap<String, McpServer>,
}

#[derive(Debug, Clone)]
pub struct Skill {
    pub name: String,
    pub description: String,
    pub body: String,
}

#[derive(Debug, Clone)]
pub struct Bundle {
    pub root: PathBuf,
    pub plugin: Value,
    pub agent: AgentFront,
    pub system_prompt: String,
    pub harness: Harness,
    pub config: Config,
    pub secrets: BTreeMap<String, SecretRef>,
    pub mcp: Option<McpConfig>,
    pub skills: Vec<Skill>,
    /// `sha256:<hex>` over all bundle files; identifies a Definition.
    pub digest: String,
}

impl Bundle {
    pub fn name(&self) -> &str {
        &self.agent.name
    }

    /// Resolve a bundle-relative path.
    pub fn path(&self, rel: &str) -> PathBuf {
        self.root.join(rel)
    }

    /// Load and validate a bundle directory.
    pub fn load(root: impl AsRef<Path>) -> Result<Bundle, BundleError> {
        let root = root.as_ref().to_path_buf();
        let mut v = Validation {
            root: root.clone(),
            issues: Vec::new(),
        };

        let plugin = v.json("plugin.json", PLUGIN_SCHEMA, true);
        let mcp_raw = v.json("mcp.json", MCP_SCHEMA, false);
        let harness_raw = v.json("x-agen/harness.json", HARNESS_SCHEMA, true);
        let config_raw = v.json("x-agen/config.json", CONFIG_SCHEMA, false);
        let secrets_raw = v.json("x-agen/secrets.json", SECRETS_SCHEMA, false);
        let agent = v.agent_md();
        let skills = v.skills();

        if let (Some(p), Some((front, _))) = (&plugin, &agent) {
            if let Some(pname) = p.get("name").and_then(Value::as_str) {
                if pname != front.name {
                    v.issue(
                        "x-agen/agent.md",
                        "/name",
                        format!("name {:?} must match plugin.json name {:?}", front.name, pname),
                    );
                }
            }
        }
        if let Some((front, _)) = &agent {
            for s in &front.skills {
                if !skills.iter().any(|k| &k.name == s) {
                    v.issue(
                        "x-agen/agent.md",
                        "/skills",
                        format!("skill {s:?} not found in skills/"),
                    );
                }
            }
        }
        let harness: Option<Harness> = harness_raw.and_then(|h| v.typed("x-agen/harness.json", h));
        if let Some(h) = &harness {
            match h.provider.as_str() {
                "fake" if h.script.is_none() => v.issue(
                    "x-agen/harness.json",
                    "/script",
                    "fake provider requires \"script\"".into(),
                ),
                "replay" if h.cassette.is_none() => v.issue(
                    "x-agen/harness.json",
                    "/cassette",
                    "replay provider requires \"cassette\"".into(),
                ),
                _ => {}
            }
            for (field, rel) in [("/script", &h.script), ("/cassette", &h.cassette)] {
                if let Some(rel) = rel {
                    if !root.join(rel).is_file() {
                        v.issue(
                            "x-agen/harness.json",
                            field,
                            format!("file {rel:?} not found in bundle"),
                        );
                    }
                }
            }
        }
        let config: Config = config_raw
            .and_then(|c| v.typed("x-agen/config.json", c))
            .unwrap_or_default();
        let secrets: BTreeMap<String, SecretRef> = secrets_raw
            .and_then(|s| v.typed("x-agen/secrets.json", s))
            .unwrap_or_default();
        let mcp: Option<McpConfig> = mcp_raw.and_then(|m| v.typed("mcp.json", m));

        if !v.issues.is_empty() {
            return Err(BundleError {
                root: root.display().to_string(),
                issues: v.issues,
            });
        }
        let (agent, system_prompt) = agent.expect("validated");
        let digest = digest_dir(&root).map_err(|e| BundleError {
            root: root.display().to_string(),
            issues: vec![Issue {
                file: String::new(),
                path: String::new(),
                message: format!("digest: {e}"),
            }],
        })?;
        Ok(Bundle {
            root,
            plugin: plugin.expect("validated"),
            agent,
            system_prompt,
            harness: harness.expect("validated"),
            config,
            secrets,
            mcp,
            skills,
            digest,
        })
    }
}

struct Validation {
    root: PathBuf,
    issues: Vec<Issue>,
}

impl Validation {
    fn issue(&mut self, file: &str, path: &str, message: String) {
        self.issues.push(Issue {
            file: file.into(),
            path: path.into(),
            message,
        });
    }

    fn read(&mut self, rel: &str, required: bool) -> Option<String> {
        let p = self.root.join(rel);
        match std::fs::read_to_string(&p) {
            Ok(s) => Some(s),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                if required {
                    self.issue(rel, "", "required file is missing".into());
                }
                None
            }
            Err(e) => {
                self.issue(rel, "", format!("cannot read: {e}"));
                None
            }
        }
    }

    fn check(&mut self, rel: &str, schema: &str, value: &Value) -> bool {
        let schema: Value = serde_json::from_str(schema).expect("embedded schema is valid JSON");
        let validator = jsonschema::validator_for(&schema).expect("embedded schema compiles");
        let before = self.issues.len();
        for err in validator.iter_errors(value) {
            let path = err.instance_path().to_string();
            self.issue(rel, &path, err.to_string());
        }
        self.issues.len() == before
    }

    fn json(&mut self, rel: &str, schema: &str, required: bool) -> Option<Value> {
        let text = self.read(rel, required)?;
        let value: Value = match serde_json::from_str(&text) {
            Ok(v) => v,
            Err(e) => {
                self.issue(rel, "", format!("invalid JSON: {e}"));
                return None;
            }
        };
        self.check(rel, schema, &value).then_some(value)
    }

    fn typed<T: serde::de::DeserializeOwned>(&mut self, rel: &str, value: Value) -> Option<T> {
        match serde_json::from_value(value) {
            Ok(t) => Some(t),
            Err(e) => {
                self.issue(rel, "", e.to_string());
                None
            }
        }
    }

    fn agent_md(&mut self) -> Option<(AgentFront, String)> {
        const REL: &str = "x-agen/agent.md";
        let text = self.read(REL, true)?;
        let Some((front, body)) = split_frontmatter(&text) else {
            self.issue(REL, "", "missing YAML frontmatter (--- ... ---)".into());
            return None;
        };
        let value: Value = match serde_yaml_ng::from_str(front) {
            Ok(v) => v,
            Err(e) => {
                self.issue(REL, "", format!("invalid frontmatter YAML: {e}"));
                return None;
            }
        };
        if !self.check(REL, AGENT_SCHEMA, &value) {
            return None;
        }
        let front: AgentFront = self.typed(REL, value)?;
        if body.trim().is_empty() {
            self.issue(REL, "", "system prompt body is empty".into());
            return None;
        }
        Some((front, body.trim().to_string()))
    }

    fn skills(&mut self) -> Vec<Skill> {
        let dir = self.root.join("skills");
        let Ok(entries) = std::fs::read_dir(&dir) else {
            return Vec::new();
        };
        let mut out = Vec::new();
        let mut dirs: Vec<_> = entries.flatten().filter(|e| e.path().is_dir()).collect();
        dirs.sort_by_key(|e| e.file_name());
        for e in dirs {
            let folder = e.file_name().to_string_lossy().to_string();
            let rel = format!("skills/{folder}/SKILL.md");
            let Some(text) = self.read(&rel, true) else {
                continue;
            };
            let Some((front, body)) = split_frontmatter(&text) else {
                self.issue(&rel, "", "missing YAML frontmatter".into());
                continue;
            };
            #[derive(Deserialize)]
            struct F {
                name: String,
                description: String,
            }
            match serde_yaml_ng::from_str::<F>(front) {
                Ok(f) if f.name == folder => out.push(Skill {
                    name: f.name,
                    description: f.description,
                    body: body.trim().to_string(),
                }),
                Ok(f) => self.issue(
                    &rel,
                    "/name",
                    format!("skill name {:?} must match folder {folder:?}", f.name),
                ),
                Err(e) => self.issue(&rel, "", format!("frontmatter needs name and description: {e}")),
            }
        }
        out
    }
}

/// Split `---\n<yaml>\n---\n<body>`.
fn split_frontmatter(text: &str) -> Option<(&str, &str)> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let rest = text.strip_prefix("---")?.trim_start_matches([' ', '\t']);
    let rest = rest.strip_prefix("\r\n").or_else(|| rest.strip_prefix('\n'))?;
    let mut offset = 0;
    for line in rest.split_inclusive('\n') {
        if line.trim_end() == "---" {
            return Some((&rest[..offset], &rest[offset + line.len()..]));
        }
        offset += line.len();
    }
    None
}

/// True for paths that are never part of a bundle (VCS, caches, editor files).
/// Must match `platform/internal/definition.Ignored`.
pub fn is_ignored(rel: &str) -> bool {
    const DIRS: [&str; 4] = [".git", "node_modules", "__pycache__", ".venv"];
    let name = rel.rsplit('/').next().unwrap_or(rel);
    rel.split('/').any(|c| DIRS.contains(&c))
        || name == ".DS_Store"
        || name.ends_with('~')
        || name.ends_with(".swp")
        || name.ends_with(".pyc")
}

/// All bundle files keyed by `/`-separated relative path (byte order).
pub fn read_files(root: &Path) -> std::io::Result<BTreeMap<String, Vec<u8>>> {
    let mut out = BTreeMap::new();
    for e in walkdir::WalkDir::new(root) {
        let e = e.map_err(std::io::Error::other)?;
        if !e.file_type().is_file() {
            continue;
        }
        let rel = e
            .path()
            .strip_prefix(root)
            .unwrap_or(e.path())
            .to_string_lossy()
            .replace('\\', "/");
        if !is_ignored(&rel) {
            out.insert(rel, std::fs::read(e.path())?);
        }
    }
    Ok(out)
}

/// Definition digest: sha256 over each file in UTF-8 byte order of its path,
/// as `path || 0x00 || u64le(len) || bytes`. Must match the Go implementation
/// in `platform/internal/definition` (shared golden vector in both test suites).
pub fn digest_files(files: &BTreeMap<String, Vec<u8>>) -> String {
    let mut h = Sha256::new();
    for (rel, bytes) in files {
        h.update(rel.as_bytes());
        h.update([0]);
        h.update((bytes.len() as u64).to_le_bytes());
        h.update(bytes);
    }
    format!("sha256:{}", hex::encode(h.finalize()))
}

pub fn digest_dir(root: &Path) -> std::io::Result<String> {
    Ok(digest_files(&read_files(root)?))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn example(name: &str) -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../../examples/bundles")
            .join(name)
    }

    fn copy_dir(src: &Path, dst: &Path) {
        for e in walkdir::WalkDir::new(src).into_iter().flatten() {
            let rel = e.path().strip_prefix(src).unwrap();
            let target = dst.join(rel);
            if e.file_type().is_dir() {
                std::fs::create_dir_all(&target).unwrap();
            } else {
                std::fs::copy(e.path(), &target).unwrap();
            }
        }
    }

    fn temp_hello() -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        copy_dir(&example("hello"), dir.path());
        dir
    }

    #[test]
    fn loads_hello_example() {
        let b = Bundle::load(example("hello")).unwrap();
        assert_eq!(b.name(), "hello");
        assert_eq!(b.harness.provider, "fake");
        assert_eq!(b.skills.len(), 1);
        assert_eq!(b.skills[0].name, "greeting");
        assert!(b.system_prompt.starts_with("You are a friendly assistant"));
        assert_eq!(b.config.permissions.default, Action::Ask);
        assert!(b.digest.starts_with("sha256:") && b.digest.len() == 71);
    }

    #[test]
    fn digest_golden_vector_matches_go() {
        // Same vector as platform/internal/definition/definition_test.go.
        let files: BTreeMap<String, Vec<u8>> = [
            ("plugin.json", b"{}\n".to_vec()),
            ("a-b/x", b"1".to_vec()),
            ("a/b", b"2".to_vec()),
            ("skills/s/SKILL.md", "h\u{e9}llo".as_bytes().to_vec()),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_string(), v))
        .collect();
        assert_eq!(
            digest_files(&files),
            "sha256:0a2a61b2870af2778739855371faf070f54a1497fa8408b338764d6fa24f3bb1"
        );
    }

    #[test]
    fn ignores_vcs_and_editor_files() {
        let d = temp_hello();
        let clean = Bundle::load(d.path()).unwrap().digest;
        std::fs::create_dir_all(d.path().join(".git")).unwrap();
        std::fs::write(d.path().join(".git/HEAD"), "ref").unwrap();
        std::fs::write(d.path().join("x-agen/agent.md~"), "backup").unwrap();
        assert_eq!(clean, Bundle::load(d.path()).unwrap().digest);
        assert!(is_ignored("a/.git/config") && is_ignored("x.pyc") && !is_ignored("skills/git/SKILL.md"));
    }

    #[test]
    fn digest_is_stable_and_content_sensitive() {
        let d = temp_hello();
        let a = Bundle::load(d.path()).unwrap().digest;
        assert_eq!(a, Bundle::load(d.path()).unwrap().digest);
        std::fs::write(
            d.path().join("x-agen/agent.md"),
            "---\nname: hello\ndescription: x\nskills: [greeting]\n---\nDifferent prompt.\n",
        )
        .unwrap();
        assert_ne!(a, Bundle::load(d.path()).unwrap().digest);
    }

    #[test]
    fn reports_all_issues_with_locations() {
        let d = temp_hello();
        std::fs::write(
            d.path().join("x-agen/harness.json"),
            r#"{"provider":"nope","model":"m","extra":1}"#,
        )
        .unwrap();
        std::fs::write(
            d.path().join("x-agen/config.json"),
            r#"{"permissions":{"default":"maybe"}}"#,
        )
        .unwrap();
        std::fs::write(
            d.path().join("x-agen/secrets.json"),
            r#"{"API_KEY":{"source":"env","value":"sk-123"}}"#,
        )
        .unwrap();
        std::fs::remove_file(d.path().join("plugin.json")).unwrap();
        let err = Bundle::load(d.path()).unwrap_err();
        let text = err.to_string();
        let has = |file: &str, path: &str| err.issues.iter().any(|i| i.file == file && i.path.starts_with(path));
        assert!(has("plugin.json", ""), "{text}");
        assert!(has("x-agen/harness.json", "/provider"), "{text}");
        assert!(has("x-agen/harness.json", ""), "{text}");
        assert!(has("x-agen/config.json", "/permissions/default"), "{text}");
        assert!(
            has("x-agen/secrets.json", "/API_KEY"),
            "secret values must be rejected: {text}"
        );
        assert!(err.issues.len() >= 5, "{text}");
    }

    #[test]
    fn rejects_name_mismatch_missing_skill_and_script() {
        let d = temp_hello();
        std::fs::write(
            d.path().join("x-agen/agent.md"),
            "---\nname: other\ndescription: d\nskills: [missing]\n---\nPrompt\n",
        )
        .unwrap();
        std::fs::remove_file(d.path().join("x-agen/fake-script.json")).unwrap();
        let err = Bundle::load(d.path()).unwrap_err();
        let msgs: Vec<String> = err.issues.iter().map(|i| i.to_string()).collect();
        assert!(
            msgs.iter().any(|m| m.contains("must match plugin.json name")),
            "{msgs:?}"
        );
        assert!(
            msgs.iter().any(|m| m.contains("skill \"missing\" not found")),
            "{msgs:?}"
        );
        assert!(msgs.iter().any(|m| m.contains("not found in bundle")), "{msgs:?}");
    }

    #[test]
    fn frontmatter_parsing() {
        assert_eq!(split_frontmatter("---\na: 1\n---\nbody"), Some(("a: 1\n", "body")));
        assert_eq!(
            split_frontmatter("---\r\na: 1\r\n---\r\nbody"),
            Some(("a: 1\r\n", "body"))
        );
        assert_eq!(split_frontmatter("no frontmatter"), None);
        assert_eq!(split_frontmatter("---\nunterminated"), None);
    }
}
