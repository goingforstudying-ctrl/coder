import type { Meta, StoryObj } from "@storybook/react-vite";
import { getPreferredProxy } from "#/contexts/ProxyContext";
import {
	MockPrimaryWorkspaceProxy,
	MockUserOwner,
	MockWorkspace,
	MockWorkspaceAgent,
	MockWorkspaceApp,
	MockWorkspaceProxies,
} from "#/testHelpers/entities";
import { withAuthProvider, withProxyProvider } from "#/testHelpers/storybook";
import { WorkspaceAppFrame } from "./WorkspaceAppFrame";
import type { WorkspaceAppWithAgent } from "./workspaceApps";

const meta: Meta<typeof WorkspaceAppFrame> = {
	title: "modules/apps/WorkspaceAppFrame",
	component: WorkspaceAppFrame,
	decorators: [withAuthProvider, withProxyProvider()],
	parameters: {
		layout: "fullscreen",
		user: MockUserOwner,
	},
	args: {
		workspace: MockWorkspace,
		app: buildWorkspaceApp(),
		active: true,
	},
};

export default meta;
type Story = StoryObj<typeof meta>;

export const Healthy: Story = {};

export const WithToolbar: Story = {
	args: {
		app: buildWorkspaceApp({ slug: "preview" }),
	},
};

export const WithWildcardWarning: Story = {
	decorators: [
		withProxyProvider({
			proxy: {
				...getPreferredProxy(MockWorkspaceProxies, MockPrimaryWorkspaceProxy),
				preferredWildcardHostname: "",
			},
		}),
	],
	args: {
		app: buildWorkspaceApp({ subdomain: true }),
	},
};

export const UnhealthyWithHealthcheck: Story = {
	args: {
		app: buildWorkspaceApp({
			health: "unhealthy",
			healthcheck: {
				url: "http://127.0.0.1:3000/healthz",
				interval: 5,
				threshold: 3,
			},
		}),
	},
};

export const UnhealthyWithoutHealthcheck: Story = {
	args: {
		app: buildWorkspaceApp({
			health: "unhealthy",
			healthcheck: undefined,
		}),
	},
};

export const Initializing: Story = {
	args: {
		app: buildWorkspaceApp({ health: "initializing" }),
	},
};

function buildWorkspaceApp(
	overrides: Partial<WorkspaceAppWithAgent> = {},
): WorkspaceAppWithAgent {
	return {
		...MockWorkspaceApp,
		agent: MockWorkspaceAgent,
		health: "healthy",
		...overrides,
	};
}
