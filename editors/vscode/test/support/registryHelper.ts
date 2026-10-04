import { configureRegistryWriter, nativeRegistryWriter } from "../../src/clusters/atomicWrite.js";
declare const __MEMQL_REGISTRY_TEST_HELPER__: string;
configureRegistryWriter(nativeRegistryWriter(__MEMQL_REGISTRY_TEST_HELPER__));
