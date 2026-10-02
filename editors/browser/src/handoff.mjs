/** A URL carries only a resource reference, never content, commands or credentials. */
export function requestedResource(pageURL) {
  const page = new URL(pageURL);
  const resources = page.searchParams.getAll('resource');
  if (resources.length === 0) return undefined;
  if (resources.length !== 1 || !page.hostname.startsWith('vscode.')) throw new Error('Invalid MemQL file link. Open the file again from MemQL OS.');
  const resource = new URL(resources[0]);
  const parts = resource.pathname.split('/').filter(Boolean).map(decodeURIComponent);
  if (resource.protocol !== 'memql-file:' || resource.username || resource.password || resource.port || resource.search || resource.hash ||
      resource.hostname !== page.hostname.slice('vscode.'.length) || parts.length !== 3 ||
      !['artifacts', 'templates'].includes(parts[0]) || !/^[\w-]{1,160}$/.test(parts[1]) || !parts[2] || /[\\/\u0000-\u001f\u007f]/.test(parts[2])) {
    throw new Error('This file link does not belong to this MemQL installation. Open it again from MemQL OS.');
  }
  return resource.href;
}

export async function checkTools(execute, timeoutMs = 20000) {
  let timer;
  try {
    const result = await Promise.race([
      execute('memql.productivity.checkTools'),
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('MemQL tools are taking too long to start. Try again or continue in basic mode.')), timeoutMs); }),
    ]);
    if (!result?.coreActive || !result.productivityActive || result.connectionVersion !== 1) {
      throw new Error('MemQL and Productivity Tools must both be active to open and save this MemQL file.');
    }
  } finally { clearTimeout(timer); }
}
