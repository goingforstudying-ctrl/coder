import { AgentSetupNotice } from "./AgentSetupNotice";

export interface AgentSetupNoticeConfig {
	canConfigureAgentSetup: boolean;
	providerCount: number;
	modelCount: number;
}

const shouldRenderAgentSetupNotice = ({
	canConfigureAgentSetup,
	providerCount,
	modelCount,
}: AgentSetupNoticeConfig) => {
	if (!canConfigureAgentSetup) {
		return modelCount === 0;
	}

	return providerCount === 0 || modelCount === 0;
};

export const getAgentSetupNoticeConfig = (
	config: AgentSetupNoticeConfig,
): AgentSetupNoticeConfig | undefined => {
	return shouldRenderAgentSetupNotice(config) ? config : undefined;
};

export const AgentSetupNoticeBanner = ({
	canConfigureAgentSetup,
	providerCount,
	modelCount,
}: AgentSetupNoticeConfig) => {
	if (
		!shouldRenderAgentSetupNotice({
			canConfigureAgentSetup,
			providerCount,
			modelCount,
		})
	) {
		return null;
	}

	if (!canConfigureAgentSetup) {
		return (
			<AgentSetupNotice isAdmin={false} providerCount={0} modelCount={0} />
		);
	}

	return (
		<AgentSetupNotice
			isAdmin
			providerCount={providerCount}
			modelCount={modelCount}
		/>
	);
};
