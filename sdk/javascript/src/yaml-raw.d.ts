// Lets a test import a YAML file as text through the "?raw" suffix of Vite,
// which the parity test uses to read api/openapi.yaml without Node typings.
declare module '*.yaml?raw' {
  const text: string;
  export default text;
}
