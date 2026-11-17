// Test-only native host. Production bundles contain none of these files.
export async function resolve(specifier,context,nextResolve) {
  if(['herdr','gpui-kit','gpui-base'].includes(specifier))return {url:new URL('./native_mock.mjs',import.meta.url).href,shortCircuit:true};
  return nextResolve(specifier,context);
}
