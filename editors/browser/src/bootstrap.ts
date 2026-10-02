import './style.css';
const setup = document.querySelector<HTMLElement>('#setup')!;
const status = document.querySelector<HTMLElement>('#setup-status')!;
const retry = document.querySelector<HTMLButtonElement>('#retry')!;
let basic = new URL(location.href).searchParams.get('mode') === 'basic';
function continueBasic() {
  const url = new URL(location.href);
  if (url.searchParams.get('mode') !== 'basic') {
    url.searchParams.set('mode', 'basic');
    location.replace(url.href);
    return;
  }
  basic = true;
  setup.hidden = true;
  document.querySelector<HTMLElement>('#basic-banner')!.hidden = false;
  document.body.classList.add('basic-mode');
}
function retryTools() {
  const url = new URL(location.href);
  url.searchParams.delete('mode');
  location.replace(url.href);
}
document.querySelector('#basic')!.addEventListener('click', continueBasic);
document.querySelector('#enable-tools')!.addEventListener('click', retryTools);
retry.addEventListener('click', retryTools);
if (basic) status.textContent = "Starting the basic editor…";
try {
  const { start } = await import('./main');
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([
      start(() => basic, () => { setup.hidden = true; }),
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error('The editor did not finish starting. Retry, or continue without MemQL tools.')), 45000);
      }),
    ]);
  } finally { clearTimeout(timer); }
  if (basic) continueBasic();
} catch (error) {
  status.textContent = error instanceof Error ? error.message : String(error);
  document.querySelector('#setup-title')!.textContent = 'MemQL tools could not start';
  document.querySelector('#setup-detail')!.textContent = 'Basic mode can edit local files. MemQL files, connected tools and versioned saves require both extensions.';
  retry.hidden = false;
}
