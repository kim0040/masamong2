<script lang="ts">
  import { t } from '../lib/i18n.svelte'
  import { logout } from '../lib/api.svelte'
  import { navigate } from '../lib/router.svelte'
  import Confirm from './Confirm.svelte'
  import SigninHelp from './SigninHelp.svelte'

  // Signing out ends this browser's session; getting back in needs the terminal on the computer
  // running Masamong, so the dialog shows how before it happens.
  let { open = $bindable(false) }: { open?: boolean } = $props()
  async function go() {
    await logout()
    navigate('/login?signed_out=1')
  }
</script>

<Confirm bind:open title={t('signin.logout_title')} body={t('signin.logout_body')} confirmLabel={t('topbar.logout')} onconfirm={go}>
  {#snippet details()}<SigninHelp />{/snippet}
</Confirm>
