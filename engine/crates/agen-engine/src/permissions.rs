//! Tool permission policy: rules evaluated deny → ask → allow, first match in
//! that order wins; otherwise the policy default applies. A rule may name a
//! tool as the agent registers it (`server.tool`) or as the model sees it
//! (`server_tool`).

use crate::bundle::{Action, Permissions};

/// Glob with `*` matching any run of characters (including dots).
pub fn glob_match(pattern: &str, name: &str) -> bool {
    let parts: Vec<&str> = pattern.split('*').collect();
    if parts.len() == 1 {
        return pattern == name;
    }
    let mut rest = name;
    for (i, part) in parts.iter().enumerate() {
        if i == 0 {
            let Some(r) = rest.strip_prefix(part) else {
                return false;
            };
            rest = r;
        } else if i == parts.len() - 1 {
            return rest.ends_with(part);
        } else {
            match rest.find(part) {
                Some(pos) => rest = &rest[pos + part.len()..],
                None => return false,
            }
        }
    }
    true
}

pub fn evaluate(policy: &Permissions, tool: &str) -> Action {
    let wire = crate::provider::openai::wire_name(tool);
    for action in [Action::Deny, Action::Ask, Action::Allow] {
        if policy
            .rules
            .iter()
            .any(|r| r.action == action && (glob_match(&r.tool, tool) || glob_match(&r.tool, &wire)))
        {
            return action;
        }
    }
    policy.default
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bundle::PermissionRule;

    fn rule(tool: &str, action: Action) -> PermissionRule {
        PermissionRule {
            tool: tool.into(),
            action,
        }
    }

    #[test]
    fn globbing() {
        assert!(glob_match("exchange.*", "exchange.place_order"));
        assert!(glob_match("*", "anything"));
        assert!(glob_match("*.get_*", "exchange.get_price"));
        assert!(!glob_match("exchange.*", "bank.pay"));
        assert!(glob_match("exact", "exact"));
        assert!(!glob_match("exact", "exactly"));
    }

    #[test]
    fn deny_beats_ask_beats_allow_regardless_of_order() {
        let p = Permissions {
            approval_timeout: None,
            default: Action::Ask,
            rules: vec![
                rule("exchange.*", Action::Allow),
                rule("exchange.place_order", Action::Ask),
                rule("*.withdraw", Action::Deny),
            ],
        };
        assert_eq!(evaluate(&p, "exchange.get_price"), Action::Allow);
        assert_eq!(evaluate(&p, "exchange.place_order"), Action::Ask);
        assert_eq!(evaluate(&p, "exchange.withdraw"), Action::Deny);
        assert_eq!(evaluate(&p, "other.tool"), Action::Ask);
        let open = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![],
        };
        assert_eq!(evaluate(&open, "x"), Action::Allow);
    }

    #[test]
    fn rules_match_the_name_the_model_sees() {
        let p = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![rule("exchange_place_order", Action::Deny), rule("bank_*", Action::Ask)],
        };
        assert_eq!(evaluate(&p, "exchange.place_order"), Action::Deny);
        assert_eq!(evaluate(&p, "bank.pay"), Action::Ask);
        assert_eq!(evaluate(&p, "exchange.get_price"), Action::Allow);
    }
}
