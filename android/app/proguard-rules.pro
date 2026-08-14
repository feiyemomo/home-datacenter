# Retrofit 2 uses FastAdapter-style reflective calls; keep its model classes.
-keepattributes Signature, InnerClasses, EnclosingMethod, *Annotation*

# kotlinx.serialization
-keepattributes RuntimeVisibleAnnotations, AnnotationDefault
-keep,includedescriptorclasses class com.example.homecenter.**$$serializer { *; }
-keepclassmembers class com.example.homecenter.** {
    *** Companion;
}
-keepclasseswithmembers class com.example.homecenter.** {
    kotlinx.serialization.KSerializer serializer(...);
}

# OkHttp / Retrofit
-dontwarn okhttp3.**
-dontwarn okio.**
-dontwarn retrofit2.**
-keep class retrofit2.** { *; }
-keep class okhttp3.** { *; }
-keepclassmembers class * {
    @retrofit2.http.* <methods>;
}