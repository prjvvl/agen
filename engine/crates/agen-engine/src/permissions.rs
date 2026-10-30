//! Tool permission policy: rules evaluated deny → ask → allow, first match in
//! that order wins; otherwise the policy default applies. A rule may name a
//! tool as the agent registers it (`server.tool`) or as the model sees it
//! (`server_tool`).

use crate::bundle::{Action, Permissions, Trust};

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

pub fn evaluate(policy: &Permissions, tool: &str, trust: Trust) -> Action {
    let wire = crate::provider::openai::wire_name(tool);
    for action in [Action::Deny, Action::Ask, Action::Allow] {
        if policy.rules.iter().any(|r| {
            r.action == action && r.matches_trust(trust) && (glob_match(&r.tool, tool) || glob_match(&r.tool, &wire))
        }) {
            return action;
        }
    }
    policy.default
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bundle::{PermissionRule, When};

    fn rule(tool: &str, action: Action) -> PermissionRule {
        PermissionRule {
            tool: tool.into(),
            action,
            when: None,
        }
    }

    fn rule_when(tool: &str, action: Action, when: When) -> PermissionRule {
        PermissionRule {
            tool: tool.into(),
            action,
            when: Some(when),
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
        assert_eq!(evaluate(&p, "exchange.get_price", Trust::Trusted), Action::Allow);
        assert_eq!(evaluate(&p, "exchange.place_order", Trust::Trusted), Action::Ask);
        assert_eq!(evaluate(&p, "exchange.withdraw", Trust::Trusted), Action::Deny);
        assert_eq!(evaluate(&p, "other.tool", Trust::Trusted), Action::Ask);
        let open = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![],
        };
        assert_eq!(evaluate(&open, "x", Trust::Trusted), Action::Allow);
    }

    #[test]
    fn rules_match_the_name_the_model_sees() {
        let p = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![rule("exchange_place_order", Action::Deny), rule("bank_*", Action::Ask)],
        };
        assert_eq!(evaluate(&p, "exchange.place_order", Trust::Trusted), Action::Deny);
        assert_eq!(evaluate(&p, "bank.pay", Trust::Trusted), Action::Ask);
        assert_eq!(evaluate(&p, "exchange.get_price", Trust::Trusted), Action::Allow);
    }

    #[test]
    fn trust_order_is_operator_above_trusted_above_untrusted() {
        assert!(Trust::Untrusted < Trust::Trusted);
        assert!(Trust::Trusted < Trust::Operator);
    }

    #[test]
    fn a_rule_conditioned_on_trust_only_matches_that_trust() {
        let p = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![rule_when(
                "*",
                Action::Ask,
                When {
                    trust: Some(Trust::Untrusted),
                    trust_below: None,
                },
            )],
        };
        assert_eq!(evaluate(&p, "x", Trust::Untrusted), Action::Ask);
        assert_eq!(evaluate(&p, "x", Trust::Trusted), Action::Allow);
        assert_eq!(evaluate(&p, "x", Trust::Operator), Action::Allow);
    }

    #[test]
    fn a_rule_conditioned_on_trust_below_matches_anything_lower() {
        // The lethal-trifecta rule: ask before acting outward once a run
        // has read untrusted content.
        let p = Permissions {
            approval_timeout: None,
            default: Action::Allow,
            rules: vec![rule_when(
                "*",
                Action::Ask,
                When {
                    trust: None,
                    trust_below: Some(Trust::Trusted),
                },
            )],
        };
        assert_eq!(evaluate(&p, "x", Trust::Untrusted), Action::Ask);
        assert_eq!(evaluate(&p, "x", Trust::Trusted), Action::Allow);
        assert_eq!(evaluate(&p, "x", Trust::Operator), Action::Allow);
    }
}
