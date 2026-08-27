export function SignOut({ onSignOut }: { onSignOut: () => void }) {
  return (
    <button type="button" className="os-sign-out" data-sign-out onClick={onSignOut}>
      Sign out
    </button>
  );
}
