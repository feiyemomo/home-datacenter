import client from "./client";
import type {
    AutomationRule,
    AutomationRulesResponse,
    AutomationMetrics,
    RuleCondition,
    RuleAction,
    RuleThrottle,
} from "@/types";

export interface CreateRulePayload {
    name: string;
    trigger: string;
    condition?: RuleCondition;
    action: RuleAction;
    throttle?: RuleThrottle;
    enabled?: boolean;
}

export interface UpdateRulePayload {
    name?: string;
    trigger?: string;
    condition?: RuleCondition;
    action?: RuleAction;
    throttle?: RuleThrottle;
    enabled?: boolean;
}

export async function listRules(): Promise<AutomationRule[]> {
    const { data } = await client.get<AutomationRulesResponse>("/automation/rules");
    return data?.rules ?? [];
}

export async function getRule(id: number): Promise<AutomationRule> {
    const { data } = await client.get<AutomationRule>(`/automation/rules/${id}`);
    return data;
}

export async function createRule(payload: CreateRulePayload): Promise<AutomationRule> {
    const { data } = await client.post<AutomationRule>("/automation/rules", payload);
    return data;
}

export async function updateRule(id: number, payload: UpdateRulePayload): Promise<AutomationRule> {
    const { data } = await client.put<AutomationRule>(`/automation/rules/${id}`, payload);
    return data;
}

export async function deleteRule(id: number): Promise<void> {
    await client.delete(`/automation/rules/${id}`);
}

export interface TestRuleResponse {
    id: number;
    name: string;
    action: string;
    test_fired: boolean;
}

export async function testRule(id: number): Promise<TestRuleResponse> {
    const { data } = await client.post<TestRuleResponse>(`/automation/rules/${id}/test`);
    return data;
}

export async function getAutomationMetrics(reset?: boolean): Promise<AutomationMetrics> {
    const { data } = await client.get<AutomationMetrics>(
        reset ? "/automation/metrics?reset=1" : "/automation/metrics"
    );
    return data;
}

export async function setRuleCooldown(id: number, seconds: number): Promise<void> {
    await client.post(`/automation/rules/${id}/cooldown`, { seconds });
}
