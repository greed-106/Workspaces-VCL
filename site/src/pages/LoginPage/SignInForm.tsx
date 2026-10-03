import type { AuthMethods } from "#/api/typesGenerated";
import { Alert } from "#/components/Alert/Alert";
import { ErrorAlert } from "#/components/Alert/ErrorAlert";
import { getApplicationName } from "#/utils/appearance";
import { PasswordSignInForm } from "./PasswordSignInForm";

type SignInFormProps = {
	isSigningIn: boolean;
	redirectTo: string;
	error?: unknown;
	message?: React.ReactNode;
	authMethods?: AuthMethods;
	onSubmit: (credentials: { email: string; password: string }) => void;
};

export const SignInForm: React.FC<SignInFormProps> = ({
	authMethods,
	isSigningIn,
	error,
	message,
	onSubmit,
}) => {
	const passwordEnabled = authMethods?.password.enabled ?? true;
	const applicationName = getApplicationName();

	return (
		<div className="w-full">
			<h1 className="text-3xl font-semibold m-0 mb-8 leading-none">
				{applicationName}
			</h1>

			{Boolean(error) && (
				<div className="mb-8">
					<ErrorAlert error={error} showDebugDetail={false} />
				</div>
			)}

			{message && (
				<div className="mb-8">
					<Alert severity="info">{message}</Alert>
				</div>
			)}

			{passwordEnabled && (
				<PasswordSignInForm
					onSubmit={onSubmit}
					autoFocus
					isSigningIn={isSigningIn}
				/>
			)}

			{!passwordEnabled && (
				<Alert severity="error" prominent>
					No authentication methods configured!
				</Alert>
			)}
		</div>
	);
};
