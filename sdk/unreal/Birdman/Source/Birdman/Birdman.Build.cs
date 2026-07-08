// Birdman UE plugin module. DRAFT — has never been compiled against a real
// engine; first build happens during game integration (see README.md).
using System.IO;
using UnrealBuildTool;

public class Birdman : ModuleRules
{
	public Birdman(ReadOnlyTargetRules Target) : base(Target)
	{
		PCHUsage = PCHUsageMode.UseExplicitOrSharedPCHs;

		PublicDependencyModuleNames.AddRange(new string[]
		{
			"Core",
			"CoreUObject",
			"Engine",
		});
		// NB: UBirdmanMatchmakingClient will need "HTTP" + "Json" once its
		// implementation lands (game-integration iteration) — not pulled yet.

		// sdk/core as an external static lib. Expected layout: this plugin
		// lives in the birdman repo (or the repo is vendored/submoduled), so
		// ModuleDirectory = <repo>/sdk/unreal/Birdman/Source/Birdman.
		string SdkRoot = Path.GetFullPath(Path.Combine(ModuleDirectory, "..", "..", "..", ".."));
		string CoreInclude = Path.Combine(SdkRoot, "core", "include");
		// Prebuilt per-platform core (see README.md "Building the core for UE"):
		//   sdk/core/build-ue/libbirdman_core.a
		string CoreLib = Path.Combine(SdkRoot, "core", "build-ue", "libbirdman_core.a");

		bool bPosix = Target.Platform == UnrealTargetPlatform.Linux
			|| Target.Platform == UnrealTargetPlatform.LinuxArm64
			|| Target.Platform == UnrealTargetPlatform.Mac;

		if (bPosix && File.Exists(CoreLib))
		{
			PublicIncludePaths.Add(CoreInclude);
			PublicAdditionalLibraries.Add(CoreLib);
			PublicDefinitions.Add("BIRDMAN_WITH_CORE=1");
		}
		else
		{
			// Editor on Windows, or core not built: the subsystem compiles to
			// safe no-ops (IsManaged() == false) — mirrors the SDK's own
			// no-op mode. Managed servers ship Linux, where core is linked.
			PublicDefinitions.Add("BIRDMAN_WITH_CORE=0");
		}
	}
}
